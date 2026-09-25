package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
	"rssfeeder/internal/web"
)

const secret = "0123456789abcdef0123456789abcdef"

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	store *storage.Store
	token string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := storage.New(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(Config{JWTSecret: secret, Password: "hunter2", Static: web.Handler()}, store, nil)
	s.failDelay = 0
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	h := &harness{t: t, srv: srv, store: store}
	var out map[string]any
	if code := h.do("POST", "/auth/login", map[string]any{"password": "hunter2"}, &out); code != 200 {
		t.Fatalf("login: %d", code)
	}
	h.token = out["token"].(string)
	return h
}

func (h *harness) req(method, path string, body any, hdr map[string]string) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// do sends an authenticated (Bearer) JSON request and decodes the response.
func (h *harness) do(method, path string, body, out any) int {
	h.t.Helper()
	hdr := map[string]string{}
	if h.token != "" {
		hdr["Authorization"] = "Bearer " + h.token
	}
	resp := h.req(method, path, body, hdr)
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestLoginRateLimitAndTokens(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < loginPerIP; i++ {
		if resp := h.req("POST", "/auth/login", map[string]string{"password": "nope"}, nil); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if resp := h.req("POST", "/auth/login", map[string]string{"password": "hunter2"}, nil); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("after %d failures even the right password must wait: %d", loginPerIP, resp.StatusCode)
	}
	// Existing tokens keep working while logins are throttled.
	if code := h.do("GET", "/counts", nil, nil); code != 200 {
		t.Fatalf("token during lockout: %d", code)
	}
	// Tokens: 90-day expiry, tampering and alg=none rejected.
	claims := &jwt.RegisteredClaims{}
	jwt.ParseWithClaims(h.token, claims, func(*jwt.Token) (any, error) { return []byte(secret), nil })
	if days := claims.ExpiresAt.Sub(claims.IssuedAt.Time).Hours() / 24; days != 90 {
		t.Fatalf("token lifetime %v days", days)
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	for _, bad := range []string{h.token + "x", none} {
		if resp := h.req("GET", "/counts", nil, map[string]string{"Authorization": "Bearer " + bad}); resp.StatusCode != 401 {
			t.Fatalf("bad token accepted: %d", resp.StatusCode)
		}
	}
}

func TestCookieSessionNeedsCSRFHeaderForWrites(t *testing.T) {
	h := newHarness(t)
	resp := h.req("POST", "/auth/login", map[string]any{"password": "hunter2", "cookie": true}, nil)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v", cookie)
	}
	withCookie := func(method, path string, body any, extra map[string]string) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(b))
		req.AddCookie(cookie)
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := withCookie("GET", "/counts", nil, nil); code != 200 {
		t.Fatalf("cookie read: %d", code)
	}
	if code := withCookie("POST", "/folders", map[string]string{"name": "x"}, nil); code != 403 {
		t.Fatalf("cookie write without CSRF header: %d", code)
	}
	if code := withCookie("POST", "/folders", map[string]string{"name": "x"}, map[string]string{csrfHeader: "web"}); code != 201 {
		t.Fatalf("cookie write with header: %d", code)
	}
}

func TestSubscribeByDiscoveryAndDuplicates(t *testing.T) {
	var feed *httptest.Server
	feed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<!doctype html><html><head><link rel="alternate" type="application/atom+xml" href="/atom"></head></html>`)
			return
		}
		fmt.Fprintf(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>Site Feed</title><entry><id>1</id><title>Hi</title><link href="%s/1"/></entry></feed>`, feed.URL)
	}))
	defer feed.Close()
	h := newHarness(t)
	var out struct {
		Source model.Source `json:"source"`
	}
	if code := h.do("POST", "/sources", map[string]string{"url": feed.URL + "/"}, &out); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if out.Source.URL != feed.URL+"/atom" || out.Source.Title != "Site Feed" || out.Source.Type != model.SourceTypeRSS {
		t.Fatalf("discovered source: %+v", out.Source)
	}
	if code := h.do("POST", "/sources", map[string]string{"url": feed.URL + "/atom", "type": "rss"}, nil); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	// Move to a folder and back to the root with an explicit null.
	var f model.Folder
	h.do("POST", "/folders", map[string]string{"name": "News"}, &f)
	var src model.Source
	h.do("PUT", fmt.Sprintf("/sources/%d", out.Source.ID), map[string]any{"folder_id": f.ID}, &src)
	if src.FolderID == nil || *src.FolderID != f.ID {
		t.Fatalf("move into folder: %+v", src.FolderID)
	}
	var moved model.Source
	h.do("PUT", fmt.Sprintf("/sources/%d", out.Source.ID), json.RawMessage(`{"folder_id": null}`), &moved)
	if moved.ID == 0 || moved.FolderID != nil {
		t.Fatal("folder_id null did not move source to root")
	}
}

func TestFolderCycleAndDepth(t *testing.T) {
	h := newHarness(t)
	var a, b, c model.Folder
	h.do("POST", "/folders", map[string]any{"name": "A"}, &a)
	h.do("POST", "/folders", map[string]any{"name": "B", "parent_id": a.ID}, &b)
	h.do("POST", "/folders", map[string]any{"name": "C", "parent_id": b.ID}, &c)
	if code := h.do("POST", "/folders", map[string]any{"name": "D", "parent_id": c.ID}, nil); code != 422 {
		t.Fatalf("4th level: %d", code)
	}
	if code := h.do("PUT", fmt.Sprintf("/folders/%d", a.ID), map[string]any{"parent_id": c.ID}, nil); code != 422 {
		t.Fatalf("cycle: %d", code)
	}
	var x model.Folder
	h.do("POST", "/folders", map[string]any{"name": "X"}, &x)
	if code := h.do("PUT", fmt.Sprintf("/folders/%d", a.ID), map[string]any{"parent_id": x.ID}, nil); code != 422 {
		t.Fatalf("moving a 3-level subtree under another folder must exceed depth: %d", code)
	}
}

func TestItemsTimelineStateAndSync(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src := &model.Source{Type: model.SourceTypeRSS, URL: "https://a.example/feed", Title: "A"}
	h.store.CreateSource(ctx, src)
	for i := 0; i < 5; i++ {
		h.store.UpsertItem(ctx, &model.Item{SourceID: src.ID, GUID: fmt.Sprint(i), URL: fmt.Sprintf("https://a.example/%d", i),
			Title: fmt.Sprintf("Item %d", i), ContentHTML: "<p>x</p>", PublishedAt: int64(1000 + i)})
	}
	var page struct {
		Items []model.ItemView `json:"items"`
		Next  string           `json:"next_cursor"`
	}
	h.do("GET", "/items?limit=2", nil, &page)
	if len(page.Items) != 2 || page.Items[0].Title != "Item 4" || page.Next == "" || page.Items[0].ContentHTML != "" {
		t.Fatalf("page 1: %+v next=%q", page.Items, page.Next)
	}
	h.do("GET", "/items?limit=2&cursor="+page.Next, nil, &page)
	if page.Items[0].Title != "Item 2" {
		t.Fatalf("page 2: %+v", page.Items)
	}

	// Swipe → state op; mark-all with undo.
	id := page.Items[0].ID
	var st struct {
		States []model.ItemState `json:"states"`
	}
	h.do("POST", "/items/state", map[string]any{"ops": []map[string]any{{"id": id, "is_starred": true}}}, &st)
	if len(st.States) != 1 || !st.States[0].IsStarred {
		t.Fatalf("bulk state: %+v", st)
	}
	var marked struct {
		IDs []int64 `json:"ids"`
	}
	h.do("POST", "/items/mark-read", map[string]any{"source_id": src.ID}, &marked)
	if len(marked.IDs) != 5 {
		t.Fatalf("mark-read: %v", marked.IDs)
	}
	h.do("POST", "/items/mark-read", map[string]any{"ids": marked.IDs, "read": false}, &marked)
	var counts storage.Counts
	h.do("GET", "/counts", nil, &counts)
	if counts.Unread != 5 || counts.Starred != 1 {
		t.Fatalf("counts after undo: %+v", counts)
	}

	// Sync pages exactly; legacy ?since=<unix> still works.
	var d model.SyncDelta
	h.do("GET", "/sync?cursor=0&limit=3", nil, &d)
	if !d.HasMore || d.Cursor == 0 {
		t.Fatalf("sync page: %+v", d)
	}
	seen := map[int64]bool{}
	cursor := int64(0)
	for {
		var p model.SyncDelta
		h.do("GET", fmt.Sprintf("/sync?cursor=%d&limit=3", cursor), nil, &p)
		for _, it := range p.Items {
			seen[it.ID] = true
		}
		cursor = p.Cursor
		if !p.HasMore {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("synced %d items", len(seen))
	}
	if code := h.do("GET", "/sync?since=1000000000", nil, &d); code != 200 || len(d.Items) == 0 {
		t.Fatalf("legacy since: %d %d items", code, len(d.Items))
	}

	// Voice round trip over HTTP.
	var v struct {
		Speak   []string       `json:"speak"`
		Context map[string]any `json:"context"`
	}
	h.do("POST", "/ask", map[string]any{"text": "what's new"}, &v)
	if len(v.Speak) == 0 || !strings.Contains(v.Speak[0], "5 new stories") {
		t.Fatalf("ask: %q", v.Speak)
	}
	h.do("POST", "/ask", map[string]any{"text": "read the first one", "context": v.Context}, &v)
	if !strings.HasPrefix(v.Speak[0], "Item 4.") {
		t.Fatalf("follow-up: %q", v.Speak)
	}
	var sp struct {
		Chunks []string `json:"chunks"`
	}
	h.do("GET", fmt.Sprintf("/items/%d/speech", id), nil, &sp)
	if len(sp.Chunks) != 2 || sp.Chunks[1] != "x" {
		t.Fatalf("speech: %q", sp.Chunks)
	}
}

func TestWebClientAndHealth(t *testing.T) {
	h := newHarness(t)
	for path, want := range map[string]int{"/": 200, "/item/5": 200, "/nope.js": 404, "/healthz": 200} {
		resp := h.req("GET", path, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s: no CSP", path)
		}
	}
	if resp := h.req("GET", "/counts", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated API: %d", resp.StatusCode)
	}
}
