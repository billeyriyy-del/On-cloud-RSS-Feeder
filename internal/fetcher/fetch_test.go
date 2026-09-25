package fetcher

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.New(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addSource(t *testing.T, s *storage.Store, u string, typ model.SourceType) *model.Source {
	t.Helper()
	src := &model.Source{Type: typ, URL: u, Title: u}
	if err := s.CreateSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSource(context.Background(), src.ID)
	return got
}

func reload(t *testing.T, s *storage.Store, id int64) *model.Source {
	t.Helper()
	src, err := s.GetSource(context.Background(), id)
	if err != nil || src == nil {
		t.Fatalf("reload %d: %v", id, err)
	}
	return src
}

const rssDoc = `<?xml version="1.0"?><rss version="2.0"><channel><title>Test Feed</title><link>%s/</link>
<item><title>One</title><link>%s/one</link><guid>1</guid></item>
<item><title>Two</title><link>%s/two</link><guid>2</guid></item></channel></rss>`

func TestPollConditionalGETAndMetadata(t *testing.T) {
	var hits, conditional int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, rssDoc, srv.URL, srv.URL, srv.URL)
	}))
	defer srv.Close()
	s := testStore(t)
	src := addSource(t, s, srv.URL+"/feed", model.SourceTypeRSS)
	f := newRSSFetcher(s, newHTTPClient())
	ctx := context.Background()

	if ids := f.Poll(ctx, src); len(ids) != 2 {
		t.Fatalf("first poll new = %v", ids)
	}
	src = reload(t, s, src.ID)
	if src.ETag != `"v1"` || src.Title != "Test Feed" || src.SiteURL != srv.URL+"/" || src.PollInterval != 1800 {
		t.Fatalf("after poll: %+v", src)
	}
	if ids := f.Poll(ctx, src); len(ids) != 0 || conditional != 1 {
		t.Fatalf("second poll new=%v conditional=%d", ids, conditional)
	}
	if src = reload(t, s, src.ID); src.PollInterval != 2700 || src.ConsecFails != 0 {
		t.Fatalf("304 should back off politely: %+v", src)
	}
}

func TestPollFollowsPermanentRedirectAndSelfHeals(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
		case "/temp":
			http.Redirect(w, r, "/new", http.StatusFound)
		case "/new", "/feed.xml":
			fmt.Fprintf(w, rssDoc, srv.URL, srv.URL, srv.URL)
		case "/page": // the old feed URL now serves the site's homepage
			fmt.Fprint(w, `<!doctype html><html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head><body>hi</body></html>`)
		}
	}))
	defer srv.Close()
	s := testStore(t)
	f := newRSSFetcher(s, newHTTPClient())
	ctx := context.Background()

	perm := addSource(t, s, srv.URL+"/old", model.SourceTypeRSS)
	f.Poll(ctx, perm)
	if got := reload(t, s, perm.ID).URL; got != srv.URL+"/new" {
		t.Fatalf("301 not followed: %s", got)
	}
	temp := addSource(t, s, srv.URL+"/temp", model.SourceTypeRSS)
	f.Poll(ctx, temp)
	if got := reload(t, s, temp.ID).URL; got != srv.URL+"/temp" {
		t.Fatalf("302 must not move the source: %s", got)
	}
	page := addSource(t, s, srv.URL+"/page", model.SourceTypeRSS)
	f.Poll(ctx, page)
	if got := reload(t, s, page.ID); got.URL != srv.URL+"/feed.xml" || got.ConsecFails != 0 {
		t.Fatalf("self-heal: %+v", got)
	}
}

func TestPollRateLimitAndGoneAndTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/busy":
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/gone":
			w.WriteHeader(http.StatusGone)
		case "/huge":
			io.WriteString(w, "<rss>"+strings.Repeat("x", MaxBody)+"</rss>")
		}
	}))
	defer srv.Close()
	s := testStore(t)
	f := newRSSFetcher(s, newHTTPClient())
	ctx := context.Background()

	busy := addSource(t, s, srv.URL+"/busy", model.SourceTypeRSS)
	before := time.Now().Unix()
	f.Poll(ctx, busy)
	if got := reload(t, s, busy.ID); got.ConsecFails != 0 || got.NextPollAt < before+7200 || got.LastError == "" {
		t.Fatalf("429: %+v", got)
	}
	gone := addSource(t, s, srv.URL+"/gone", model.SourceTypeRSS)
	f.Poll(ctx, gone)
	if !reload(t, s, gone.ID).IsDead {
		t.Fatal("410 should retire the feed")
	}
	huge := addSource(t, s, srv.URL+"/huge", model.SourceTypeRSS)
	f.Poll(ctx, huge)
	if got := reload(t, s, huge.ID); got.ConsecFails != 1 || !strings.Contains(got.LastError, "larger than") {
		t.Fatalf("5 MB guard: %+v", got)
	}
}

func TestDiscover(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog":
			fmt.Fprint(w, `<!doctype html><html><head><title>Blog</title>
<link rel="alternate" type="application/rss+xml" title="Comments" href="/comments.xml">
<link rel="alternate" type="application/atom+xml" title="Posts" href="posts.atom">
</head><body></body></html>`)
		case "/posts.atom", "/comments.xml", "/plain/feed":
			fmt.Fprintf(w, rssDoc, srv.URL, srv.URL, srv.URL)
		case "/plain/":
			fmt.Fprint(w, `<!doctype html><html><head><title>Plain site</title></head><body></body></html>`)
		case "/nothing":
			fmt.Fprint(w, `<!doctype html><html><head><title>No feeds here</title></head><body></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := NewDiscoverer()
	ctx := context.Background()

	c, err := d.Discover(ctx, srv.URL+"/blog")
	if err != nil || len(c) != 2 || c[0].URL != srv.URL+"/posts.atom" || c[0].Via != "link" || c[0].Items != 2 {
		t.Fatalf("link discovery (comments last): %+v %v", c, err)
	}
	c, err = d.Discover(ctx, srv.URL+"/blog")
	if c[0].Title != "Test Feed" {
		t.Fatalf("title from feed: %+v", c[0])
	}
	c, err = d.Discover(ctx, srv.URL+"/plain/")
	if err != nil || len(c) != 1 || c[0].Via != "guess" {
		t.Fatalf("well-known path: %+v %v", c, err)
	}
	c, err = d.Discover(ctx, srv.URL+"/nothing")
	if err != nil || len(c) != 1 || c[0].Type != model.SourceTypeHTML || c[0].Title != "No feeds here" {
		t.Fatalf("scrape fallback: %+v %v", c, err)
	}
	c, err = d.Discover(ctx, srv.URL+"/posts.atom")
	if err != nil || c[0].Via != "direct" {
		t.Fatalf("direct: %+v %v", c, err)
	}
}

func TestSiteRules(t *testing.T) {
	d := NewDiscoverer()
	cases := map[string]string{
		"https://www.youtube.com/channel/UCBR8-60-B28hp2BmDPdntcQ": "https://www.youtube.com/feeds/videos.xml?channel_id=UCBR8-60-B28hp2BmDPdntcQ",
		"https://www.youtube.com/playlist?list=PL123":              "https://www.youtube.com/feeds/videos.xml?playlist_id=PL123",
		"https://old.reddit.com/r/golang/":                         "https://www.reddit.com/r/golang/.rss",
		"https://github.com/golang/go":                             "https://github.com/golang/go/releases.atom",
		"https://github.com/rsc":                                   "https://github.com/rsc.atom",
		"https://medium.com/@someone/some-post-123":                "https://medium.com/feed/@someone",
		"https://example.substack.com/p/hello":                     "https://example.substack.com/feed",
		"https://mastodon.social/@Gargron":                         "https://mastodon.social/@Gargron.rss",
		"https://bsky.app/profile/example.com":                     "https://bsky.app/profile/example.com/rss",
	}
	for in, want := range cases {
		u, _ := url.Parse(in)
		got, err := d.siteRules(context.Background(), u)
		if err != nil || len(got) == 0 || got[0] != want {
			t.Errorf("%s → %v, want %s", in, got, want)
		}
	}
	for _, in := range []string{"example.com", "feed://example.com/rss", "  https://example.com  "} {
		if _, err := normalizeInput(in); err != nil {
			t.Errorf("normalizeInput(%q): %v", in, err)
		}
	}
	if _, err := normalizeInput("javascript:alert(1)"); err == nil {
		t.Error("javascript: accepted")
	}
}

const articlePage = `<!doctype html><html lang="en"><head><title>A Long Read | Site</title>
<meta property="og:image" content="/hero.jpg"><meta property="article:published_time" content="2023-10-30T08:00:00Z">
</head><body><nav><a href="/">Home</a><a href="/tag/x">tag</a></nav>
<article><h1>A Long Read</h1><p class="byline">By Jane Doe</p>%s</article><footer>© Site</footer></body></html>`

func longParas() string {
	var b strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "<p>Paragraph %d explains, at some length and with several clauses, why readable extraction must keep the body text and drop the navigation, the footer, and the share buttons around it.</p>", i)
	}
	return b.String()
}

func TestFullTextExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/paywall" {
			fmt.Fprint(w, `<html><body><p>Subscribe to continue reading.</p></body></html>`)
			return
		}
		fmt.Fprintf(w, articlePage, longParas())
	}))
	defer srv.Close()
	s := testStore(t)
	src := addSource(t, s, srv.URL+"/feed", model.SourceTypeRSS)
	id, _, _ := s.UpsertItem(context.Background(), &model.Item{SourceID: src.ID, GUID: "g", URL: srv.URL + "/a", Title: "A Long Read", ContentHTML: "<p>teaser</p>", ContentText: "teaser", PublishedAt: 1})
	ex := NewExtractor()

	if err := FetchFullText(context.Background(), s, ex, id, srv.URL+"/a"); err != nil {
		t.Fatal(err)
	}
	it, _ := s.GetItem(context.Background(), id, false)
	if !it.HasFullText || !strings.Contains(it.ContentHTML, "Paragraph 11") || strings.Contains(it.ContentHTML, "© Site") {
		t.Fatalf("full text: has=%v %q", it.HasFullText, it.ContentHTML[:min(200, len(it.ContentHTML))])
	}
	if r, _ := s.SearchItems(context.Background(), "extraction", 5); len(r) != 1 {
		t.Fatal("full text not indexed")
	}
	if orig, _ := s.GetItem(context.Background(), id, true); orig.ContentHTML != "<p>teaser</p>" {
		t.Fatalf("original content lost: %q", orig.ContentHTML)
	}

	id2, _, _ := s.UpsertItem(context.Background(), &model.Item{SourceID: src.ID, GUID: "p", URL: srv.URL + "/paywall", Title: "P", PublishedAt: 1})
	if err := FetchFullText(context.Background(), s, ex, id2, srv.URL+"/paywall"); err == nil {
		t.Fatal("paywall page accepted as article")
	}
	if st, _ := s.FullTextStatus(context.Background(), id2); st != "failed" {
		t.Fatalf("failure not recorded: %q", st)
	}
}

func TestScraperPoll(t *testing.T) {
	var srv *httptest.Server
	var indexHits int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog/":
			atomic.AddInt32(&indexHits, 1)
			if r.Header.Get("If-None-Match") == `"idx"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"idx"`)
			fmt.Fprint(w, `<html><head><title>Old Blog</title></head><body><nav><a href="/blog/">Home</a><a href="/tag/go">go</a><a href="/about">about</a></nav>
<main><a href="/blog/2023/first-post">First</a><a href="/blog/2023/second-post#comments">Second</a><a href="https://other.example/x">ext</a><a href="/img/cat.png">img</a></main></body></html>`)
		default:
			fmt.Fprintf(w, articlePage, longParas())
		}
	}))
	defer srv.Close()
	s := testStore(t)
	src := addSource(t, s, srv.URL+"/blog/", model.SourceTypeHTML)
	h := newHTMLScraper(s, newHTTPClient())
	ctx := context.Background()

	ids := h.Poll(ctx, src)
	if len(ids) != 2 {
		t.Fatalf("scraped %d articles, want 2 (nav, tags, assets and other hosts excluded)", len(ids))
	}
	it, _ := s.GetItem(ctx, ids[0], false)
	if !strings.HasPrefix(it.Title, "A Long Read") || it.Author != "Jane Doe" || it.ImageURL != srv.URL+"/hero.jpg" || it.PublishedAt != 1698652800 {
		t.Fatalf("article metadata: title=%q author=%q image=%q published=%d", it.Title, it.Author, it.ImageURL, it.PublishedAt)
	}
	src = reload(t, s, src.ID)
	if src.ETag != `"idx"` || src.Title != "Old Blog" {
		t.Fatalf("scraper must store ETag and title: %+v", src)
	}
	if ids := h.Poll(ctx, src); len(ids) != 0 || reload(t, s, src.ID).ConsecFails != 0 {
		t.Fatal("304 on the index must be a success, not a failure")
	}
}

func TestWebSubFlow(t *testing.T) {
	var subscribed url.Values
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		subscribed = r.PostForm
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()
	s := testStore(t)
	sched := NewScheduler(s, Options{PublicURL: "https://noema.example"})
	ws := sched.WebSub()
	src := addSource(t, s, "https://blog.example/feed", model.SourceTypeRSS)
	ctx := context.Background()

	ws.maybeSubscribe(ctx, src, hub.URL, "https://blog.example/feed")
	if subscribed.Get("hub.mode") != "subscribe" || subscribed.Get("hub.callback") != fmt.Sprintf("https://noema.example/websub/%d", src.ID) {
		t.Fatalf("subscribe request: %v", subscribed)
	}
	if reload(t, s, src.ID).Push {
		t.Fatal("push must not be active before verification")
	}

	// Hub verifies intent.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/websub/x?hub.mode=subscribe&hub.topic="+url.QueryEscape("https://blog.example/feed")+"&hub.challenge=abc123&hub.lease_seconds=3600", nil)
	ws.HandleVerify(rec, req, src.ID)
	if rec.Code != 200 || rec.Body.String() != "abc123" || !reload(t, s, src.ID).Push {
		t.Fatalf("verify: %d %q", rec.Code, rec.Body.String())
	}
	// A verification we did not ask for (replayed or forged) is refused.
	rec = httptest.NewRecorder()
	ws.HandleVerify(rec, httptest.NewRequest("GET", "/websub/x?hub.mode=subscribe&hub.topic="+url.QueryEscape("https://blog.example/feed")+"&hub.challenge=again&hub.lease_seconds=999999999", nil), src.ID)
	if rec.Code != 404 {
		t.Fatalf("unsolicited verification accepted: %d", rec.Code)
	}
	// Wrong topic is refused.
	rec = httptest.NewRecorder()
	ws.HandleVerify(rec, httptest.NewRequest("GET", "/websub/x?hub.mode=subscribe&hub.topic=https://evil/&hub.challenge=z", nil), src.ID)
	if rec.Code != 404 {
		t.Fatalf("foreign topic verified: %d", rec.Code)
	}

	// Content delivery with and without a valid signature.
	body := fmt.Sprintf(rssDoc, "https://blog.example", "https://blog.example", "https://blog.example")
	push := func(sig string) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/websub/x", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/rss+xml")
		r.Header.Set("X-Hub-Signature-256", sig)
		ws.HandlePush(rec, r, src.ID)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("push status %d", rec.Code)
		}
	}
	push("sha256=deadbeef")
	time.Sleep(100 * time.Millisecond)
	if c, _ := s.GetCounts(ctx); c.Unread != 0 {
		t.Fatal("unsigned push ingested")
	}
	m := hmac.New(sha256.New, []byte(subscribed.Get("hub.secret")))
	m.Write([]byte(body))
	push("sha256=" + hex.EncodeToString(m.Sum(nil)))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, _ := s.GetCounts(ctx); c.Unread == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("signed push not ingested")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
