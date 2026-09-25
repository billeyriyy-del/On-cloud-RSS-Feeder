package fetcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
	"rssfeeder/internal/storage"
)

// maxArticlesPerPoll bounds a first scrape of a big archive page; the rest are
// picked up on later polls, a few at a time, with polite gaps between requests.
const maxArticlesPerPoll = 15

// HTMLScraper fetches HTML-only blogs using a two-phase index→article approach.
type HTMLScraper struct {
	store *storage.Store
	http  *httpClient
}

func newHTMLScraper(store *storage.Store, hc *httpClient) *HTMLScraper {
	return &HTMLScraper{store: store, http: hc}
}

// Poll scrapes the source's index page, discovers new article URLs, and
// fetches them. Returns ids of new items.
func (s *HTMLScraper) Poll(ctx context.Context, src *model.Source) []int64 {
	Metrics.Add("polls", 1)
	cfg := src.ScraperConfig
	if cfg == nil {
		cfg = &model.ScraperConfig{} // heuristics only
	}
	resp, err := s.http.get(ctx, src.URL, acceptHTML, src.ETag, src.LastModified)
	if err != nil {
		recordFailure(ctx, s.store, src, err, 0)
		return nil
	}
	now := time.Now().Unix()
	if resp.PermanentURL != "" {
		if moved, _ := s.store.MoveSourceURL(ctx, src.ID, resp.PermanentURL); moved {
			src.URL = resp.PermanentURL
		}
	}
	switch resp.Status {
	case http.StatusOK:
	case http.StatusNotModified:
		s.success(ctx, src, now, false, src.ETag, src.LastModified)
		return nil
	default:
		recordFailure(ctx, s.store, src, fmt.Errorf("HTTP %d", resp.Status), resp.RetryAfter)
		return nil
	}

	articleURLs, err := extractLinks(resp.FinalURL, resp.Body, resp.Header.Get("Content-Type"), cfg.IndexSelector)
	if err != nil {
		recordFailure(ctx, s.store, src, fmt.Errorf("extract links: %w", err), 0)
		return nil
	}
	existing, err := s.store.GetItemURLsForSource(ctx, src.ID)
	if err != nil {
		recordFailure(ctx, s.store, src, err, 0)
		return nil
	}
	var todo []string
	for _, u := range articleURLs {
		if !existing[u] {
			todo = append(todo, u)
		}
		if len(todo) >= maxArticlesPerPoll {
			break
		}
	}

	var items []*model.Item
	for i, articleURL := range todo {
		if i > 0 {
			select { // polite inter-request delay: 500–1000 ms
			case <-ctx.Done():
				return nil
			case <-time.After(time.Duration(500+rand.Intn(500)) * time.Millisecond):
			}
		}
		item, err := s.fetchArticle(ctx, src.ID, articleURL, cfg, now)
		if err != nil {
			slog.Warn("scrape article", "url", articleURL, "err", err)
			continue
		}
		items = append(items, item)
	}
	newIDs := s.store.UpsertItems(ctx, items)
	Metrics.Add("items_ingested", int64(len(newIDs)))
	if src.Title == "" || src.Title == src.URL {
		s.store.UpdateFeedMeta(ctx, src.ID, model.FeedMeta{Title: pageTitle(resp.Body), SiteURL: resp.FinalURL})
	}
	s.success(ctx, src, now, len(newIDs) > 0, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"))
	return newIDs
}

func (s *HTMLScraper) success(ctx context.Context, src *model.Source, now int64, hadNew bool, etag, lastMod string) {
	interval := adaptInterval(src.PollInterval, hadNew)
	s.store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt: now, NextPollAt: now + jitter(interval), PollInterval: interval,
		ETag: etag, LastModified: lastMod,
	})
}

func (s *HTMLScraper) fetchArticle(ctx context.Context, sourceID int64, articleURL string, cfg *model.ScraperConfig, now int64) (*model.Item, error) {
	resp, err := s.http.get(ctx, articleURL, acceptHTML, "", "")
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.Status)
	}
	pageURL, err := url.Parse(resp.FinalURL)
	if err != nil {
		return nil, err
	}
	a, err := readable(resp.Body, resp.Header.Get("Content-Type"), pageURL)
	if err != nil {
		return nil, err
	}
	content := a.HTML
	if cfg.ContentSelector != "" {
		if sel := selectHTML(resp.Body, resp.Header.Get("Content-Type"), cfg.ContentSelector); sel != "" {
			content = normalize.SanitizeHTML(normalize.ResolveURLs(sel, pageURL))
		}
	}
	text := normalize.HTMLToText(content)

	published := now
	if a.Published != nil && a.Published.Unix() > 0 && a.Published.Unix() <= now+maxFutureSkew {
		published = a.Published.Unix()
	}
	if cfg.DateSelector != "" {
		if t, ok := selectDate(resp.Body, resp.Header.Get("Content-Type"), cfg.DateSelector); ok {
			published = t
		}
	}
	title := a.Title
	if title == "" {
		title = normalize.Truncate(text, 80)
	}
	summary := a.Excerpt
	if summary == "" {
		summary = text
	}
	image := a.Image
	if image == "" {
		image = normalize.FirstImage(content)
	}
	return &model.Item{
		SourceID:    sourceID,
		GUID:        fmt.Sprintf("%x", sha256.Sum256([]byte(articleURL))),
		URL:         articleURL,
		Title:       title,
		Author:      a.Byline,
		Summary:     normalize.Truncate(summary, 280),
		ImageURL:    image,
		Kind:        model.KindArticle,
		ReadingSecs: normalize.ReadingSecs(text),
		PublishedAt: published,
		FetchedAt:   now,
		ContentHTML: content,
		ContentText: text,
	}, nil
}

func parseHTML(body []byte, contentType string) (*html.Node, error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, err
	}
	return html.Parse(r)
}

// Paths that are navigation, not articles.
var navSegments = map[string]bool{
	"tag": true, "tags": true, "category": true, "categories": true, "page": true, "author": true,
	"search": true, "feed": true, "rss": true, "login": true, "signin": true, "signup": true,
	"about": true, "contact": true, "privacy": true, "terms": true, "archive": true, "archives": true,
	"subscribe": true, "wp-login.php": true, "cdn-cgi": true,
}

var assetExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".svg": true, ".pdf": true,
	".xml": true, ".css": true, ".js": true, ".zip": true, ".mp3": true, ".mp4": true,
}

// extractLinks returns same-host article-looking links from the index page, in
// document order, deduplicated. A selector (if given) scopes the search.
func extractLinks(baseURL string, body []byte, contentType, selector string) ([]string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	doc, err := parseHTML(body, contentType)
	if err != nil {
		return nil, err
	}
	roots := []*html.Node{doc}
	if selector != "" {
		if sel, err := cascadia.ParseGroup(selector); err == nil {
			if matches := cascadia.QueryAll(doc, sel); len(matches) > 0 {
				roots = matches
			}
		}
	}
	basePath := strings.TrimSuffix(base.Path, "/")
	seen := map[string]bool{}
	var links []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key != "href" {
					continue
				}
				href := strings.TrimSpace(a.Val)
				if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") {
					break
				}
				u, err := url.Parse(href)
				if err != nil {
					break
				}
				r := base.ResolveReference(u)
				r.Fragment = ""
				if r.Host != base.Host || !isArticlePath(r.Path, basePath) {
					break
				}
				s := normalize.StripTrackingParams(r.String())
				if !seen[s] {
					seen[s] = true
					links = append(links, s)
				}
				break
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return links, nil
}

func isArticlePath(p, indexPath string) bool {
	clean := strings.TrimSuffix(p, "/")
	if clean == "" || clean == indexPath {
		return false
	}
	if assetExt[strings.ToLower(path.Ext(clean))] {
		return false
	}
	for _, seg := range strings.Split(strings.Trim(clean, "/"), "/") {
		if navSegments[strings.ToLower(seg)] {
			return false
		}
	}
	return true
}

// selectHTML returns the rendered inner HTML of the first selector match.
func selectHTML(body []byte, contentType, selector string) string {
	doc, err := parseHTML(body, contentType)
	if err != nil {
		return ""
	}
	sel, err := cascadia.ParseGroup(selector)
	if err != nil {
		return ""
	}
	node := cascadia.Query(doc, sel)
	if node == nil {
		return ""
	}
	var buf bytes.Buffer
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		html.Render(&buf, c) // escapes text and attributes correctly
	}
	return buf.String()
}

var dateLayouts = []string{
	time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
	"January 2, 2006", "Jan 2, 2006", "2 January 2006", "2 Jan 2006", "02/01/2006", time.RFC1123, time.RFC1123Z,
}

// selectDate reads a date from the first selector match: its datetime/content
// attribute if present, else its text.
func selectDate(body []byte, contentType, selector string) (int64, bool) {
	doc, err := parseHTML(body, contentType)
	if err != nil {
		return 0, false
	}
	sel, err := cascadia.ParseGroup(selector)
	if err != nil {
		return 0, false
	}
	node := cascadia.Query(doc, sel)
	if node == nil {
		return 0, false
	}
	var candidates []string
	for _, a := range node.Attr {
		if a.Key == "datetime" || a.Key == "content" {
			candidates = append(candidates, a.Val)
		}
	}
	var sb strings.Builder
	var text func(*html.Node)
	text = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			text(c)
		}
	}
	text(node)
	candidates = append(candidates, strings.TrimSpace(sb.String()))
	for _, c := range candidates {
		for _, l := range dateLayouts {
			if t, err := time.Parse(l, strings.TrimSpace(c)); err == nil {
				return t.Unix(), true
			}
		}
	}
	return 0, false
}
