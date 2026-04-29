package fetcher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"

	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
	"rssfeeder/internal/storage"
)

// HTMLScraper fetches HTML-only blogs using a two-phase index→article approach.
type HTMLScraper struct {
	store  *storage.Store
	client *http.Client
}

func newHTMLScraper(store *storage.Store) *HTMLScraper {
	return &HTMLScraper{
		store:  store,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Poll scrapes the source's index page, discovers new article URLs, and fetches them.
func (s *HTMLScraper) Poll(ctx context.Context, src *model.Source) {
	if src.ScraperConfig == nil {
		slog.Warn("html source missing scraper_config", "id", src.ID)
		return
	}
	cfg := src.ScraperConfig

	body, finalURL, err := s.fetch(ctx, src.URL, src.ETag, src.LastModified)
	if err != nil {
		s.recordFailure(ctx, src, err)
		return
	}

	articleURLs, err := s.extractLinks(finalURL, body, cfg.IndexSelector)
	if err != nil {
		s.recordFailure(ctx, src, fmt.Errorf("extract links: %w", err))
		return
	}

	// Normalize and deduplicate against stored items.
	existing, err := s.store.GetItemURLsForSource(ctx, src.ID)
	if err != nil {
		s.recordFailure(ctx, src, err)
		return
	}
	var newURLs []string
	seen := make(map[string]bool)
	for _, u := range articleURLs {
		u = normalize.StripTrackingParams(u)
		if !existing[u] && !seen[u] {
			seen[u] = true
			newURLs = append(newURLs, u)
		}
	}

	now := time.Now().Unix()
	newCount := 0

	for i, articleURL := range newURLs {
		if i > 0 {
			// Polite inter-request delay: 500–1000 ms.
			delay := time.Duration(500+rand.Intn(500)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		item, err := s.fetchArticle(ctx, src.ID, articleURL, cfg, now)
		if err != nil {
			slog.Warn("scrape article", "url", articleURL, "err", err)
			continue
		}

		_, isNew, err := s.store.UpsertItem(ctx, item)
		if err != nil {
			slog.Error("store scraped item", "err", err)
			continue
		}
		if isNew {
			newCount++
		}
	}

	newInterval := adaptInterval(src.PollInterval, newCount > 0)
	s.store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt:   now,
		NextPollAt:   now + newInterval,
		PollInterval: newInterval,
		ConsecFails:  0,
		IsDead:       false,
	})
}

func (s *HTMLScraper) fetchArticle(
	ctx context.Context,
	sourceID int64,
	articleURL string,
	cfg *model.ScraperConfig,
	fetchedAt int64,
) (*model.Item, error) {
	body, pageURL, err := s.fetch(ctx, articleURL, "", "")
	if err != nil {
		return nil, err
	}

	var title, contentHTML, author string

	if cfg.ContentSelector != "" {
		title, contentHTML, author = extractBySelector(body, cfg.ContentSelector)
	}

	// Fall back to heuristic extraction when selectors are absent or produced nothing.
	if contentHTML == "" {
		title, contentHTML, author = extractHeuristic(body, pageURL)
	}

	contentHTML = normalize.SanitizeHTML(contentHTML)
	h := sha256.Sum256([]byte(articleURL + title))

	return &model.Item{
		SourceID:    sourceID,
		GUID:        fmt.Sprintf("%x", h),
		URL:         articleURL,
		Title:       title,
		Author:      author,
		PublishedAt: fetchedAt,
		FetchedAt:   fetchedAt,
		ContentHTML: contentHTML,
		ContentText: normalize.HTMLToText(contentHTML),
	}, nil
}

func (s *HTMLScraper) fetch(ctx context.Context, rawURL, etag, lastMod string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		req.Header.Set("If-Modified-Since", lastMod)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", "", err
	}
	return string(body), resp.Request.URL.String(), nil
}

// extractLinks returns all same-host links from the index page.
// If selector is non-empty it scopes the search to matching elements.
func (s *HTMLScraper) extractLinks(baseURL, body, selector string) ([]string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	root := doc
	if selector != "" {
		if sel, err := cascadia.ParseGroup(selector); err == nil {
			if match := cascadia.Query(doc, sel); match != nil {
				root = match
			}
		}
	}

	var links []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					href := strings.TrimSpace(a.Val)
					if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
						break
					}
					u, err := url.Parse(href)
					if err != nil {
						break
					}
					resolved := base.ResolveReference(u)
					if resolved.Host == base.Host {
						resolved.Fragment = ""
						links = append(links, resolved.String())
					}
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return links, nil
}

// extractBySelector extracts content using a CSS selector.
func extractBySelector(body, selector string) (title, content, author string) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return
	}
	sel, err := cascadia.ParseGroup(selector)
	if err != nil {
		return
	}
	node := cascadia.Query(doc, sel)
	if node == nil {
		return
	}
	content = renderNode(node)
	return
}

// extractHeuristic pulls title from <title> and body content from <article> or <main>
// or falls back to the largest <div> by text length.
func extractHeuristic(body, _ string) (title, content, author string) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", body, ""
	}

	// Extract <title>
	if sel, err := cascadia.Parse("title"); err == nil {
		if n := cascadia.Query(doc, sel); n != nil && n.FirstChild != nil {
			title = strings.TrimSpace(n.FirstChild.Data)
		}
	}

	// Try semantic content containers in priority order.
	for _, candidate := range []string{"article", "main", "[role=main]"} {
		if sel, err := cascadia.Parse(candidate); err == nil {
			if n := cascadia.Query(doc, sel); n != nil {
				content = renderNode(n)
				return
			}
		}
	}

	// Fall back: collect all <p> text.
	if sel, err := cascadia.Parse("p"); err == nil {
		var sb strings.Builder
		for _, n := range cascadia.QueryAll(doc, sel) {
			sb.WriteString(renderNode(n))
		}
		content = sb.String()
	}
	return
}

// renderNode serializes an HTML subtree back to an HTML string.
func renderNode(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			sb.WriteString(n.Data)
		case html.ElementNode:
			sb.WriteByte('<')
			sb.WriteString(n.Data)
			for _, a := range n.Attr {
				sb.WriteByte(' ')
				sb.WriteString(a.Key)
				sb.WriteString(`="`)
				sb.WriteString(a.Val)
				sb.WriteByte('"')
			}
			sb.WriteByte('>')
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			sb.WriteString("</")
			sb.WriteString(n.Data)
			sb.WriteByte('>')
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c)
	}
	return sb.String()
}

func (s *HTMLScraper) recordFailure(ctx context.Context, src *model.Source, err error) {
	slog.Warn("scrape failure", "source", src.ID, "url", src.URL, "err", err)
	fails := src.ConsecFails + 1
	now := time.Now().Unix()
	s.store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt:   now,
		NextPollAt:   now + backoffSecs(fails),
		PollInterval: src.PollInterval,
		ConsecFails:  fails,
		IsDead:       fails >= maxConsecFails,
	})
}
