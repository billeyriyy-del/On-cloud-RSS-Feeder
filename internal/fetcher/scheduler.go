package fetcher

import (
	"context"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
)

// Options configure the scheduler.
type Options struct {
	Retention time.Duration // hard cutoff for unstarred items (default 90 days)
	PublicURL string        // enables WebSub when set
	BackupDir string        // daily VACUUM INTO backups when set
	Keep      int           // backups to keep (default 7)
}

// Scheduler owns all outbound fetching. It is deliberately one goroutine
// polling sources one after another: at ~300 feeds with conditional GETs the
// whole cycle is seconds of work, and sequential polling needs no locking,
// cannot stampede a host, and keeps memory flat on a 1 GB instance.
type Scheduler struct {
	store     *storage.Store
	rss       *RSSFetcher
	html      *HTMLScraper
	extractor Extractor
	websub    *WebSub
	opts      Options

	kick     chan struct{}
	fulltext chan int64 // source ids with freshly pushed items to prefetch
}

// NewScheduler constructs a Scheduler. Call Run to start it.
func NewScheduler(store *storage.Store, opts Options) *Scheduler {
	if opts.Retention <= 0 {
		opts.Retention = 90 * 24 * time.Hour
	}
	if opts.Keep <= 0 {
		opts.Keep = 7
	}
	hc := newHTTPClient()
	s := &Scheduler{
		store:     store,
		rss:       newRSSFetcher(store, hc),
		html:      newHTMLScraper(store, hc),
		extractor: &readabilityExtractor{http: hc},
		opts:      opts,
		kick:      make(chan struct{}, 1),
		fulltext:  make(chan int64, 256),
	}
	if opts.PublicURL != "" {
		s.websub = newWebSub(store, opts.PublicURL)
		s.websub.ingest = s.ingestPush
		s.rss.onHub = s.websub.maybeSubscribe
	}
	return s
}

// WebSub returns the push handler, or nil when push is disabled.
func (s *Scheduler) WebSub() *WebSub { return s.websub }

// Extractor exposes the full-text extractor for on-demand API use.
func (s *Scheduler) Extractor() Extractor { return s.extractor }

// Kick asks the loop to check for due sources now (non-blocking).
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// SpreadFirstPoll returns next_poll_at for bulk-imported sources, spread
// across [now, now+interval) to prevent an OPML-import thundering herd.
func SpreadFirstPoll(interval int64) int64 {
	return time.Now().Unix() + rand.Int63n(interval+1)
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	pollTicker := time.NewTicker(time.Minute)
	maintTicker := time.NewTicker(6 * time.Hour)
	defer pollTicker.Stop()
	defer maintTicker.Stop()

	s.pollDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			s.pollDue(ctx)
		case <-s.kick:
			s.pollDue(ctx)
		case id := <-s.fulltext:
			s.prefetch(ctx, id)
		case <-maintTicker.C:
			s.maintain(ctx)
		}
	}
}

func (s *Scheduler) pollDue(ctx context.Context) {
	sources, err := s.store.GetDueSources(ctx, time.Now().Unix())
	if err != nil {
		slog.Error("get due sources", "err", err)
		return
	}
	for _, src := range sources {
		if ctx.Err() != nil {
			return
		}
		newIDs := s.poll(ctx, src)
		if src.FetchFullText && len(newIDs) > 0 {
			s.prefetch(ctx, src.ID)
		}
	}
}

func (s *Scheduler) poll(ctx context.Context, src *model.Source) []int64 {
	switch src.Type {
	case model.SourceTypeRSS:
		return s.rss.Poll(ctx, src)
	case model.SourceTypeHTML:
		return s.html.Poll(ctx, src)
	default:
		slog.Warn("unknown source type", "id", src.ID, "type", src.Type)
		return nil
	}
}

// prefetch extracts full text for a source's newest unattempted items.
func (s *Scheduler) prefetch(ctx context.Context, sourceID int64) {
	items, err := s.store.ItemsNeedingFullText(ctx, sourceID, 10)
	if err != nil {
		slog.Error("full text queue", "source", sourceID, "err", err)
		return
	}
	for i, it := range items {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(500+rand.Intn(500)) * time.Millisecond):
			}
		}
		fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		if err := FetchFullText(fctx, s.store, s.extractor, it.ID, it.URL); err != nil {
			slog.Debug("full text", "item", it.ID, "err", err)
		}
		cancel()
	}
}

// ingestPush stores items delivered by a WebSub hub.
func (s *Scheduler) ingestPush(ctx context.Context, src *model.Source, body []byte, contentType string) {
	feed, err := ParseFeed(body, contentType)
	if err != nil {
		slog.Warn("websub payload", "source", src.ID, "err", err)
		s.store.RequestPoll(ctx, src.ID, 0) // fat ping unusable: poll instead
		s.Kick()
		return
	}
	items, _ := BuildItems(src.ID, feed, src.URL, time.Now().Unix())
	newIDs := s.store.UpsertItems(ctx, items)
	Metrics.Add("items_ingested", int64(len(newIDs)))
	if src.FetchFullText && len(newIDs) > 0 {
		select {
		case s.fulltext <- src.ID:
		default:
		}
	}
}

func (s *Scheduler) maintain(ctx context.Context) {
	now := time.Now()
	cutoff := now.Add(-s.opts.Retention).Unix()
	if n, err := s.store.SweepExpiredItems(ctx, cutoff); err != nil {
		slog.Error("retention sweep", "err", err)
	} else if n > 0 {
		slog.Info("retention sweep", "deleted", n)
	}
	// Deletes older than the retention window can go: a client that has been
	// offline longer than that is told to resync from scratch.
	if n, err := s.store.CompactChangeLog(ctx, cutoff); err != nil {
		slog.Error("compact change log", "err", err)
	} else if n > 0 {
		slog.Info("change log compacted", "removed", n)
	}
	if s.websub != nil {
		s.websub.Renew(ctx)
	}
	if now.Day() == 1 && now.Hour() < 6 {
		if err := s.store.VacuumDB(ctx); err != nil {
			slog.Error("VACUUM", "err", err)
		}
	}
	s.backup(ctx, now)
}

// backup writes one consistent snapshot per day and keeps the newest opts.Keep.
func (s *Scheduler) backup(ctx context.Context, now time.Time) {
	if s.opts.BackupDir == "" {
		return
	}
	if err := os.MkdirAll(s.opts.BackupDir, 0o700); err != nil {
		slog.Error("backup dir", "err", err)
		return
	}
	path := filepath.Join(s.opts.BackupDir, "noema-"+now.Format("20060102")+".db")
	if _, err := os.Stat(path); err == nil {
		return // today's exists
	}
	if err := s.store.Backup(ctx, path); err != nil {
		slog.Error("backup", "err", err)
		return
	}
	slog.Info("backup written", "path", path)
	matches, _ := filepath.Glob(filepath.Join(s.opts.BackupDir, "noema-*.db"))
	sort.Strings(matches)
	for len(matches) > s.opts.Keep {
		if strings.HasPrefix(filepath.Base(matches[0]), "noema-") {
			os.Remove(matches[0])
		}
		matches = matches[1:]
	}
}

// RunMaintenance runs one maintenance pass (exposed for tests and admin use).
func (s *Scheduler) RunMaintenance(ctx context.Context) { s.maintain(ctx) }
