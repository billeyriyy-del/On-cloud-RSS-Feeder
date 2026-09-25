package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
)

// Candidate is a feed (or scrapeable page) found for a URL the user pasted.
type Candidate struct {
	URL   string           `json:"url"`
	Title string           `json:"title"`
	Type  model.SourceType `json:"type"`
	Kind  model.ItemKind   `json:"kind"`
	Via   string           `json:"via"` // direct | rule | link | guess | scrape
	Items int              `json:"items"`
}

// Discoverer turns "any URL a person would paste" into subscribable feeds.
type Discoverer struct {
	http *httpClient
}

func NewDiscoverer() *Discoverer { return &Discoverer{http: newHTTPClient()} }

// Discover returns candidates best-first. It never returns an empty slice
// with a nil error: if nothing is found the page itself is offered for scraping.
func (d *Discoverer) Discover(ctx context.Context, raw string) ([]Candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u, err := normalizeInput(raw)
	if err != nil {
		return nil, err
	}

	var out []Candidate
	seen := map[string]bool{}
	tryAll := func(urls []string, via string) {
		for _, fu := range urls {
			if seen[fu] || len(out) >= 6 {
				continue
			}
			seen[fu] = true
			if c, ok := d.probe(ctx, fu, via); ok {
				out = append(out, c)
			}
		}
	}

	// 1. Known sites: construct the feed URL directly.
	rules, err := d.siteRules(ctx, u)
	if err != nil {
		return nil, err
	}
	tryAll(rules, "rule")
	if len(out) > 0 {
		return out, nil
	}

	// 2. The URL itself.
	resp, err := d.http.get(ctx, u.String(), acceptFeed, "", "")
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, errors.New("the page returned HTTP " + http.StatusText(resp.Status))
	}
	final := resp.FinalURL
	if feed, err := ParseFeed(resp.Body, resp.Header.Get("Content-Type")); err == nil {
		items, meta := BuildItems(0, feed, final, time.Now().Unix())
		return []Candidate{{URL: final, Title: meta.Title, Type: model.SourceTypeRSS, Kind: meta.Kind, Via: "direct", Items: len(items)}}, nil
	}

	// 3. <link rel="alternate"> in the page.
	links := feedLinksFromHTML(resp.Body, final)
	urls := make([]string, len(links))
	for i, l := range links {
		urls[i] = l.URL
	}
	tryAll(urls, "link")
	if len(out) > 0 {
		return out, nil
	}

	// 4. Conventional locations, next to the page first (blogs often live
	// under /blog/), then at the site root.
	base, _ := url.Parse(final)
	var guesses []string
	dir := base.Path
	if !strings.HasSuffix(dir, "/") {
		dir = dir[:strings.LastIndex(dir, "/")+1]
	}
	for _, p := range []string{"feed", "rss", "feed.xml", "rss.xml", "atom.xml", "index.xml", "feed.json", "?feed=rss2", "feeds/posts/default"} {
		path, query, _ := strings.Cut(p, "?")
		for _, d := range []string{dir, "/"} {
			u := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: d + path, RawQuery: query}
			guesses = append(guesses, u.String())
		}
	}
	for _, g := range guesses {
		tryAll([]string{g}, "guess")
		if len(out) > 0 {
			return out, nil
		}
	}

	// 5. Nothing: offer to scrape the page with heuristics.
	return []Candidate{{URL: final, Title: pageTitle(resp.Body), Type: model.SourceTypeHTML, Kind: model.KindArticle, Via: "scrape"}}, nil
}

func normalizeInput(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("url required")
	}
	if strings.HasPrefix(raw, "feed:") {
		raw = strings.TrimPrefix(strings.TrimPrefix(raw, "feed:"), "//")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("not a web address")
	}
	return u, nil
}

// probe fetches a URL and reports it as a candidate if it parses as a feed.
func (d *Discoverer) probe(ctx context.Context, u, via string) (Candidate, bool) {
	resp, err := d.http.get(ctx, u, acceptFeed, "", "")
	if err != nil || resp.Status != http.StatusOK {
		return Candidate{}, false
	}
	feed, err := ParseFeed(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return Candidate{}, false
	}
	items, meta := BuildItems(0, feed, resp.FinalURL, time.Now().Unix())
	title := meta.Title
	if title == "" {
		title = resp.FinalURL
	}
	return Candidate{URL: resp.FinalURL, Title: title, Type: model.SourceTypeRSS, Kind: meta.Kind, Via: via, Items: len(items)}, true
}

var (
	ytChannelRe = regexp.MustCompile(`^/channel/(UC[\w-]{22})`)
	redditRe    = regexp.MustCompile(`^/(r|user|u)/([\w-]+)`)
	ghRepoRe    = regexp.MustCompile(`^/([\w.-]+)/([\w.-]+)/?$`)
	ghUserRe    = regexp.MustCompile(`^/([\w-]+)/?$`)
	mastoRe     = regexp.MustCompile(`^/@([\w.]+)/?$`)
	bskyRe      = regexp.MustCompile(`^/profile/([\w.:-]+)`)
	applePodRe  = regexp.MustCompile(`/id(\d+)`)
)

// siteRules maps well-known sites to their feed URLs without scraping pages.
func (d *Discoverer) siteRules(ctx context.Context, u *url.URL) ([]string, error) {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	p := u.EscapedPath()
	switch {
	case host == "youtube.com" || host == "m.youtube.com":
		if m := ytChannelRe.FindStringSubmatch(p); m != nil {
			return []string{"https://www.youtube.com/feeds/videos.xml?channel_id=" + m[1]}, nil
		}
		if list := u.Query().Get("list"); list != "" {
			return []string{"https://www.youtube.com/feeds/videos.xml?playlist_id=" + url.QueryEscape(list)}, nil
		}
		// @handle, /c/, /user/: the channel page links its feed; fall through.
	case host == "reddit.com" || host == "old.reddit.com":
		if m := redditRe.FindStringSubmatch(p); m != nil {
			kind := m[1]
			if kind == "u" {
				kind = "user"
			}
			return []string{"https://www.reddit.com/" + kind + "/" + m[2] + "/.rss"}, nil
		}
	case host == "github.com":
		if m := ghRepoRe.FindStringSubmatch(p); m != nil {
			base := "https://github.com/" + m[1] + "/" + strings.TrimSuffix(m[2], ".git")
			return []string{base + "/releases.atom", base + "/tags.atom", base + "/commits.atom"}, nil
		}
		if m := ghUserRe.FindStringSubmatch(p); m != nil {
			return []string{"https://github.com/" + m[1] + ".atom"}, nil
		}
	case host == "medium.com":
		if seg := strings.Trim(p, "/"); seg != "" {
			return []string{"https://medium.com/feed/" + strings.SplitN(seg, "/", 2)[0]}, nil
		}
	case strings.HasSuffix(host, ".medium.com"):
		return []string{"https://" + host + "/feed"}, nil
	case strings.HasSuffix(host, ".substack.com"), strings.HasSuffix(host, ".tumblr.com"):
		suffix := "/feed"
		if strings.HasSuffix(host, ".tumblr.com") {
			suffix = "/rss"
		}
		return []string{"https://" + host + suffix}, nil
	case host == "bsky.app":
		if m := bskyRe.FindStringSubmatch(p); m != nil {
			return []string{"https://bsky.app/profile/" + m[1] + "/rss"}, nil
		}
	case host == "news.ycombinator.com":
		return []string{"https://news.ycombinator.com/rss"}, nil
	case host == "podcasts.apple.com":
		if m := applePodRe.FindStringSubmatch(p); m != nil {
			if feed := d.appleFeed(ctx, m[1]); feed != "" {
				return []string{feed}, nil
			}
		}
	}
	// Mastodon (and other fediverse servers) expose /@user.rss on any host.
	if m := mastoRe.FindStringSubmatch(p); m != nil {
		return []string{u.Scheme + "://" + u.Host + "/@" + m[1] + ".rss"}, nil
	}
	return nil, nil
}

// appleFeed resolves an Apple Podcasts id to the show's RSS feed.
func (d *Discoverer) appleFeed(ctx context.Context, id string) string {
	resp, err := d.http.get(ctx, "https://itunes.apple.com/lookup?id="+id, "application/json", "", "")
	if err != nil || resp.Status != http.StatusOK {
		return ""
	}
	var r struct {
		Results []struct {
			FeedURL string `json:"feedUrl"`
		} `json:"results"`
	}
	if json.Unmarshal(resp.Body, &r) != nil || len(r.Results) == 0 {
		return ""
	}
	return r.Results[0].FeedURL
}

var feedTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
	"application/json":      true,
	"application/rdf+xml":   true,
	"text/xml":              true,
	"application/xml":       true,
}

// feedLinksFromHTML returns <link rel="alternate"> feed URLs, comments feeds last.
func feedLinksFromHTML(body []byte, pageURL string) []Candidate {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	z := html.NewTokenizer(bytes.NewReader(body))
	var main, comments []Candidate
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) == "body" {
			break
		}
		if string(name) != "link" || !hasAttr {
			continue
		}
		attrs := map[string]string{}
		for {
			k, v, more := z.TagAttr()
			attrs[strings.ToLower(string(k))] = string(v)
			if !more {
				break
			}
		}
		rels := " " + strings.ToLower(attrs["rel"]) + " "
		typ := strings.ToLower(strings.TrimSpace(strings.SplitN(attrs["type"], ";", 2)[0]))
		if !strings.Contains(rels, " alternate ") || !feedTypes[typ] || attrs["href"] == "" {
			continue
		}
		ref, err := url.Parse(strings.TrimSpace(attrs["href"]))
		if err != nil {
			continue
		}
		c := Candidate{URL: base.ResolveReference(ref).String(), Title: attrs["title"], Type: model.SourceTypeRSS, Via: "link"}
		if strings.Contains(strings.ToLower(c.Title+c.URL), "comment") {
			comments = append(comments, c)
		} else {
			main = append(main, c)
		}
	}
	return append(main, comments...)
}

func pageTitle(body []byte) string {
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				if z.Next() == html.TextToken {
					return normalize.CleanTitle(string(z.Text()))
				}
				return ""
			}
		}
	}
}
