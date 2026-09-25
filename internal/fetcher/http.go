package fetcher

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxBody is the non-negotiable guard on any outbound fetch. A body larger
	// than this is an error, never silently truncated into a half-parsed feed.
	MaxBody = 5 << 20

	userAgent = "Noema/2.0 (self-hosted RSS reader; +https://github.com/billeyriyy-del/on-cloud-rss-feeder)"

	acceptFeed = "application/atom+xml, application/rss+xml, application/feed+json, application/json;q=0.9, application/xml;q=0.9, text/xml;q=0.8, text/html;q=0.7, */*;q=0.5"
	acceptHTML = "text/html, application/xhtml+xml;q=0.9, */*;q=0.5"
)

// Metrics are exported through expvar (/debug/vars).
var Metrics = expvar.NewMap("noema")

var errTooLarge = fmt.Errorf("response larger than %d bytes", MaxBody)

// response is a fully-read HTTP response.
type response struct {
	Status       int
	Body         []byte
	Header       http.Header
	FinalURL     string // after redirects
	PermanentURL string // set when every hop was 301/308: the source should move
	RetryAfter   time.Duration
}

type httpClient struct {
	c *http.Client
}

type redirectKey struct{}

// redirectLog records the status of each redirect hop for one request.
type redirectLog struct{ codes []int }

func newHTTPClient() *httpClient {
	c := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errors.New("too many redirects")
			}
			if log, ok := req.Context().Value(redirectKey{}).(*redirectLog); ok && req.Response != nil {
				log.codes = append(log.codes, req.Response.StatusCode)
			}
			return nil
		},
	}
	return &httpClient{c: c}
}

// get performs a conditional GET and reads at most MaxBody bytes.
func (h *httpClient) get(ctx context.Context, rawURL, accept, etag, lastMod string) (*response, error) {
	log := &redirectLog{}
	ctx = context.WithValue(ctx, redirectKey{}, log)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		req.Header.Set("If-Modified-Since", lastMod)
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &response{Status: resp.StatusCode, Header: resp.Header, FinalURL: resp.Request.URL.String()}
	if len(log.codes) > 0 && out.FinalURL != rawURL {
		permanent := true
		for _, c := range log.codes {
			if c != http.StatusMovedPermanently && c != http.StatusPermanentRedirect {
				permanent = false
			}
		}
		if permanent {
			out.PermanentURL = out.FinalURL
		}
	}
	out.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))

	if resp.StatusCode == http.StatusOK {
		if resp.ContentLength > MaxBody {
			return nil, errTooLarge
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
		if err != nil {
			return nil, err
		}
		if len(body) > MaxBody {
			return nil, errTooLarge
		}
		out.Body = body
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // let the connection be reused
	}
	return out, nil
}

// parseRetryAfter handles both delta-seconds and HTTP-date forms.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
