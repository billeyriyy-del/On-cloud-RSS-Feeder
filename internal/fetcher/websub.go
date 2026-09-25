package fetcher

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
)

// leaseSecs is the lease we ask hubs for; they may grant less. maxLeaseSecs
// caps what we accept so a bogus verification cannot pin a lease forever.
const (
	leaseSecs    = 10 * 24 * 3600
	maxLeaseSecs = 30 * 24 * 3600
)

type pendingSub struct {
	topic string
	at    time.Time
}

// WebSub subscribes to feeds that advertise a hub (WordPress.com, Blogger,
// Medium, YouTube, many static-site hosts via Superfeedr/websub.rocks) so new
// posts arrive in seconds instead of on the next poll. It needs a public URL
// the hub can reach; without one Noema simply keeps polling.
type WebSub struct {
	mu      sync.Mutex
	pending map[int64]pendingSub // subscriptions we asked for, awaiting verification

	store     *storage.Store
	client    *http.Client
	publicURL string
	ingest    func(ctx context.Context, src *model.Source, body []byte, contentType string)
}

func newWebSub(store *storage.Store, publicURL string) *WebSub {
	return &WebSub{
		store:     store,
		client:    &http.Client{Timeout: 20 * time.Second},
		publicURL: strings.TrimSuffix(publicURL, "/"),
		pending:   map[int64]pendingSub{},
	}
}

// expect records that we just asked a hub to (re)subscribe source id.
func (w *WebSub) expect(id int64, topic string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[id] = pendingSub{topic: topic, at: time.Now()}
}

// claim consumes a pending subscription if the hub's verification matches one
// we asked for in the last hour. Anything else is someone else's request.
func (w *WebSub) claim(id int64, topic string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.pending[id]
	if !ok || p.topic != topic || time.Since(p.at) > time.Hour {
		return false
	}
	delete(w.pending, id)
	return true
}

func (w *WebSub) callback(id int64) string {
	return w.publicURL + "/websub/" + strconv.FormatInt(id, 10)
}

// maybeSubscribe is the RSSFetcher hook: subscribe when a hub is new to us.
func (w *WebSub) maybeSubscribe(ctx context.Context, src *model.Source, hub, topic string) {
	if src.WebSub.Hub == hub && src.WebSub.Topic == topic {
		return // already subscribed or pending; renewals handle the rest
	}
	if err := w.Subscribe(ctx, src, hub, topic); err != nil {
		slog.Warn("websub subscribe", "source", src.ID, "hub", hub, "err", err)
	}
}

// Subscribe asks the hub to push topic to our callback. The subscription only
// becomes active when the hub verifies it (HandleVerify).
func (w *WebSub) Subscribe(ctx context.Context, src *model.Source, hub, topic string) error {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	sub := model.WebSub{Hub: hub, Topic: topic, Secret: hex.EncodeToString(secret), ExpiresAt: src.WebSub.ExpiresAt}
	if err := w.store.SetWebSub(ctx, src.ID, sub); err != nil {
		return err
	}
	src.WebSub = sub
	w.expect(src.ID, topic)
	return w.post(ctx, hub, url.Values{
		"hub.mode":          {"subscribe"},
		"hub.topic":         {topic},
		"hub.callback":      {w.callback(src.ID)},
		"hub.secret":        {sub.Secret},
		"hub.lease_seconds": {strconv.Itoa(leaseSecs)},
	})
}

// Unsubscribe is best effort (used when a source is deleted).
func (w *WebSub) Unsubscribe(ctx context.Context, src *model.Source) {
	if src.WebSub.Hub == "" {
		return
	}
	w.post(ctx, src.WebSub.Hub, url.Values{
		"hub.mode":     {"unsubscribe"},
		"hub.topic":    {src.WebSub.Topic},
		"hub.callback": {w.callback(src.ID)},
	})
}

func (w *WebSub) post(ctx context.Context, hub string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Renew re-subscribes leases that end within a day.
func (w *WebSub) Renew(ctx context.Context) {
	srcs, err := w.store.ListWebSubRenewals(ctx, time.Now().Add(24*time.Hour).Unix())
	if err != nil {
		slog.Error("websub renewals", "err", err)
		return
	}
	for _, src := range srcs {
		if err := w.Subscribe(ctx, src, src.WebSub.Hub, src.WebSub.Topic); err != nil {
			slog.Warn("websub renew", "source", src.ID, "err", err)
			// Fall back to polling at normal cadence until the next attempt.
			src.WebSub.ExpiresAt = 0
			w.store.SetWebSub(ctx, src.ID, src.WebSub)
		}
	}
}

// HandleVerify answers the hub's GET intent verification.
func (w *WebSub) HandleVerify(rw http.ResponseWriter, r *http.Request, id int64) {
	q := r.URL.Query()
	mode, topic, challenge := q.Get("hub.mode"), q.Get("hub.topic"), q.Get("hub.challenge")
	src, err := w.store.GetSource(r.Context(), id)
	if err != nil {
		http.Error(rw, "error", http.StatusInternalServerError)
		return
	}
	switch mode {
	case "subscribe":
		if src == nil || src.WebSub.Hub == "" || src.WebSub.Topic != topic || challenge == "" || !w.claim(id, topic) {
			http.NotFound(rw, r)
			return
		}
		lease, _ := strconv.ParseInt(q.Get("hub.lease_seconds"), 10, 64)
		if lease <= 0 {
			lease = leaseSecs
		}
		if lease > maxLeaseSecs {
			lease = maxLeaseSecs
		}
		src.WebSub.ExpiresAt = time.Now().Unix() + lease
		if err := w.store.SetWebSub(r.Context(), id, src.WebSub); err != nil {
			http.Error(rw, "error", http.StatusInternalServerError)
			return
		}
		slog.Info("websub active", "source", id, "lease_s", lease)
	case "unsubscribe":
		if src != nil && src.WebSub.Hub != "" {
			http.NotFound(rw, r) // we did not ask to leave
			return
		}
	case "denied":
		if src != nil {
			w.store.SetWebSub(r.Context(), id, model.WebSub{})
		}
		rw.WriteHeader(http.StatusOK)
		return
	default:
		http.Error(rw, "bad mode", http.StatusBadRequest)
		return
	}
	rw.Header().Set("Content-Type", "text/plain")
	io.WriteString(rw, challenge)
}

// HandlePush receives content distribution. Per the spec it answers 2xx even
// when the signature is wrong (so as not to act as an oracle) but ignores the body.
func (w *WebSub) HandlePush(rw http.ResponseWriter, r *http.Request, id int64) {
	src, err := w.store.GetSource(r.Context(), id)
	if err != nil || src == nil || src.WebSub.Hub == "" {
		http.NotFound(rw, r) // tells the hub to drop the subscription
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		http.Error(rw, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	rw.WriteHeader(http.StatusAccepted)
	if !validSignature(r.Header.Get("X-Hub-Signature-256"), r.Header.Get("X-Hub-Signature"), src.WebSub.Secret, body) {
		slog.Warn("websub bad signature", "source", id)
		Metrics.Add("websub_rejected", 1)
		return
	}
	Metrics.Add("websub_pushes", 1)
	// Ingest after replying so a slow parse never makes the hub retry.
	ct := r.Header.Get("Content-Type")
	go w.ingest(context.Background(), src, body, ct)
}

func validSignature(sig256, sig, secret string, body []byte) bool {
	if secret == "" {
		return true
	}
	header := sig256
	if header == "" {
		header = sig
	}
	algo, hexSig, ok := strings.Cut(header, "=")
	if !ok {
		return false
	}
	var h func() hash.Hash
	switch strings.ToLower(algo) {
	case "sha1":
		h = sha1.New
	case "sha256":
		h = sha256.New
	case "sha384":
		h = sha512.New384
	case "sha512":
		h = sha512.New
	default:
		return false
	}
	want, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	m := hmac.New(h, []byte(secret))
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}
