package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
)

const (
	defaultInterval int64 = 3600  // 1 h
	minInterval     int64 = 900   // 15 min
	maxInterval     int64 = 21600 // 6 h
	maxBackoff      int64 = 86400 // 24 h
	maxConsecFails        = 10
)

// RSSFetcher polls syndication feeds (RSS, Atom, JSON Feed) and stores items.
type RSSFetcher struct {
	store *storage.Store
	http  *httpClient
	// onHub is called when a feed advertises a WebSub hub (may be nil).
	onHub func(ctx context.Context, src *model.Source, hub, topic string)
}

func newRSSFetcher(store *storage.Store, hc *httpClient) *RSSFetcher {
	return &RSSFetcher{store: store, http: hc}
}

// Poll fetches src once, stores new items and poll state, and returns the ids
// of newly inserted items (for full-text prefetch).
func (f *RSSFetcher) Poll(ctx context.Context, src *model.Source) []int64 {
	Metrics.Add("polls", 1)
	resp, err := f.http.get(ctx, src.URL, acceptFeed, src.ETag, src.LastModified)
	if err != nil {
		f.recordFailure(ctx, src, err, 0)
		return nil
	}
	now := time.Now().Unix()

	if resp.PermanentURL != "" && resp.PermanentURL != src.URL {
		if moved, err := f.store.MoveSourceURL(ctx, src.ID, resp.PermanentURL); err == nil && moved {
			slog.Info("feed moved permanently", "source", src.ID, "from", src.URL, "to", resp.PermanentURL)
			src.URL = resp.PermanentURL
		}
	}

	switch resp.Status {
	case http.StatusOK:
	case http.StatusNotModified:
		f.recordSuccess(ctx, src, now, false, src.ETag, src.LastModified)
		return nil
	case http.StatusGone:
		slog.Warn("source gone", "id", src.ID, "url", src.URL)
		f.store.UpdatePollState(ctx, src.ID, model.PollState{
			LastPollAt: now, NextPollAt: now + maxBackoff, PollInterval: src.PollInterval,
			ConsecFails: src.ConsecFails + 1, LastError: "HTTP 410 Gone", IsDead: true,
		})
		return nil
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		// Being rate-limited is not the feed being broken: wait as asked,
		// without counting towards the dead-feed threshold.
		wait := int64(resp.RetryAfter / time.Second)
		if wait < src.PollInterval {
			wait = src.PollInterval
		}
		if wait > maxBackoff {
			wait = maxBackoff
		}
		f.store.UpdatePollState(ctx, src.ID, model.PollState{
			LastPollAt: now, NextPollAt: now + wait, PollInterval: src.PollInterval,
			ETag: src.ETag, LastModified: src.LastModified, ConsecFails: src.ConsecFails,
			LastError: fmt.Sprintf("HTTP %d (rate limited)", resp.Status),
		})
		return nil
	default:
		f.recordFailure(ctx, src, fmt.Errorf("HTTP %d", resp.Status), resp.RetryAfter)
		return nil
	}

	feed, err := ParseFeed(resp.Body, resp.Header.Get("Content-Type"))
	if errors.Is(err, ErrNotFeed) {
		// The URL now serves a web page. If that page links to a feed, follow it
		// (sites that redesign often move /rss to /feed.xml).
		if cands := feedLinksFromHTML(resp.Body, resp.FinalURL); len(cands) > 0 && cands[0].URL != src.URL {
			if moved, _ := f.store.MoveSourceURL(ctx, src.ID, cands[0].URL); moved {
				slog.Info("feed self-healed via autodiscovery", "source", src.ID, "to", cands[0].URL)
				f.store.UpdatePollState(ctx, src.ID, model.PollState{
					LastPollAt: now, NextPollAt: now, PollInterval: src.PollInterval,
				})
				return nil
			}
		}
		f.recordFailure(ctx, src, errors.New("URL no longer serves a feed"), 0)
		return nil
	}
	if err != nil {
		f.recordFailure(ctx, src, fmt.Errorf("parse: %w", err), 0)
		return nil
	}

	items, meta := BuildItems(src.ID, feed, src.URL, now)
	newIDs := f.store.UpsertItems(ctx, items)
	Metrics.Add("items_ingested", int64(len(newIDs)))
	if err := f.store.UpdateFeedMeta(ctx, src.ID, meta); err != nil {
		slog.Warn("update feed meta", "source", src.ID, "err", err)
	}
	if f.onHub != nil {
		if hub, self := findHub(resp.Body, resp.Header); hub != "" {
			if self == "" {
				self = src.URL
			}
			f.onHub(ctx, src, hub, self)
		}
	}
	f.recordSuccess(ctx, src, now, len(newIDs) > 0, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"))
	return newIDs
}

func (f *RSSFetcher) recordSuccess(ctx context.Context, src *model.Source, now int64, hadNew bool, etag, lastMod string) {
	interval := adaptInterval(src.PollInterval, hadNew)
	if src.Push {
		interval = maxInterval // push delivers; polling is only a safety net
	}
	f.store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt: now, NextPollAt: now + jitter(interval), PollInterval: interval,
		ETag: etag, LastModified: lastMod,
	})
}

func (f *RSSFetcher) recordFailure(ctx context.Context, src *model.Source, err error, retryAfter time.Duration) {
	recordFailure(ctx, f.store, src, err, retryAfter)
}

func recordFailure(ctx context.Context, store *storage.Store, src *model.Source, err error, retryAfter time.Duration) {
	Metrics.Add("poll_failures", 1)
	slog.Warn("poll failure", "source", src.ID, "url", src.URL, "err", err)
	fails := src.ConsecFails + 1
	now := time.Now().Unix()
	wait := backoffSecs(fails)
	if ra := int64(retryAfter / time.Second); ra > wait {
		wait = min(ra, maxBackoff)
	}
	store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt:   now,
		NextPollAt:   now + wait,
		PollInterval: src.PollInterval,
		ETag:         src.ETag,
		LastModified: src.LastModified,
		ConsecFails:  fails,
		LastError:    err.Error(),
		IsDead:       fails >= maxConsecFails,
	})
}

// adaptInterval implements the ×1.5 / ÷2 adaptive logic.
func adaptInterval(current int64, hadNew bool) int64 {
	if current <= 0 {
		current = defaultInterval
	}
	if hadNew {
		return max(current/2, minInterval)
	}
	return min(current*3/2, maxInterval)
}

// backoffSecs returns exponential backoff (1 min, 2, 4 …) capped at 24 h.
func backoffSecs(fails int) int64 {
	secs := int64(60)
	for i := 1; i < fails; i++ {
		secs *= 2
		if secs >= maxBackoff {
			return maxBackoff
		}
	}
	return secs
}
