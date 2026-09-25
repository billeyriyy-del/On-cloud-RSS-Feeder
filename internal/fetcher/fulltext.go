package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html/charset"

	"rssfeeder/internal/normalize"
	"rssfeeder/internal/storage"
)

// Extractor pulls the readable article out of a web page. It is an interface
// because github.com/go-shiori/go-readability is deprecated upstream in favour
// of codeberg.org/readeck/go-readability/v2; swapping is a one-file change.
type Extractor interface {
	Extract(ctx context.Context, pageURL string) (html, text string, err error)
}

// minArticleChars: below this the "article" is almost certainly a paywall,
// cookie wall or index page, and the feed's own content is the better choice.
const minArticleChars = 300

type readabilityExtractor struct {
	http *httpClient
}

// NewExtractor returns the default readability-based extractor.
func NewExtractor() Extractor { return &readabilityExtractor{http: newHTTPClient()} }

func (r *readabilityExtractor) Extract(ctx context.Context, pageURL string) (string, string, error) {
	resp, err := r.http.get(ctx, pageURL, acceptHTML, "", "")
	if err != nil {
		return "", "", err
	}
	if resp.Status != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.Status)
	}
	final, err := url.Parse(resp.FinalURL)
	if err != nil {
		return "", "", err
	}
	a, err := readable(resp.Body, resp.Header.Get("Content-Type"), final)
	if err != nil {
		return "", "", err
	}
	if len([]rune(a.Text)) < minArticleChars {
		return "", "", errors.New("extracted text too short (paywall or not an article)")
	}
	return a.HTML, a.Text, nil
}

// article is readability's output, sanitised and with URLs made absolute.
type article struct {
	Title, Byline, HTML, Text, Image, Excerpt string
	Published                                 *time.Time
}

func readable(body []byte, contentType string, pageURL *url.URL) (*article, error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, err
	}
	a, err := readability.FromReader(r, pageURL)
	if err != nil {
		return nil, fmt.Errorf("readability: %w", err)
	}
	content := normalize.SanitizeHTML(normalize.ResolveURLs(a.Content, pageURL))
	return &article{
		Title:     normalize.CleanTitle(a.Title),
		Byline:    trimBy(normalize.CleanTitle(a.Byline)),
		HTML:      content,
		Text:      normalize.HTMLToText(content),
		Image:     normalize.ResolveURL(pageURL.String(), a.Image),
		Excerpt:   normalize.CleanTitle(a.Excerpt),
		Published: a.PublishedTime,
	}, nil
}

// FetchFullText extracts and stores the full article for one item. It is used
// on demand by the API and by the scheduler for sources that opt in.
func FetchFullText(ctx context.Context, store *storage.Store, ex Extractor, itemID int64, pageURL string) error {
	if pageURL == "" {
		err := errors.New("item has no link")
		store.SetFullText(ctx, itemID, "", "", 0, err)
		return err
	}
	html, text, err := ex.Extract(ctx, pageURL)
	if err != nil {
		Metrics.Add("fulltext_failures", 1)
		if serr := store.SetFullText(ctx, itemID, "", "", 0, err); serr != nil {
			return serr
		}
		return err
	}
	Metrics.Add("fulltext_ok", 1)
	return store.SetFullText(ctx, itemID, html, text, normalize.ReadingSecs(text), nil)
}

// trimBy turns "By Jane Doe" into "Jane Doe".
func trimBy(s string) string {
	if len(s) > 3 && strings.EqualFold(s[:3], "by ") {
		return strings.TrimSpace(s[3:])
	}
	return s
}
