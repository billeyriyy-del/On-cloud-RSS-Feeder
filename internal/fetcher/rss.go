package fetcher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mmcdole/gofeed"

	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
	"rssfeeder/internal/storage"
)

const (
	defaultInterval int64 = 3600   // 1 h
	minInterval     int64 = 900    // 15 min
	maxInterval     int64 = 21600  // 6 h
	maxBackoff      int64 = 86400  // 24 h
	maxBody               = 10 << 20 // 10 MB
	maxConsecFails        = 10

	userAgent = "rssfeeder/1.0 (personal RSS reader; contact: change-me@example.com)"
)

// RSSFetcher polls RSS/Atom sources and stores normalized items.
type RSSFetcher struct {
	store  *storage.Store
	client *http.Client
}

func newRSSFetcher(store *storage.Store) *RSSFetcher {
	return &RSSFetcher{
		store:  store,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Poll fetches src once, parses the feed, and writes new items + updated poll state.
func (f *RSSFetcher) Poll(ctx context.Context, src *model.Source) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		f.recordFailure(ctx, src, err)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	if src.ETag != "" {
		req.Header.Set("If-None-Match", src.ETag)
	}
	if src.LastModified != "" {
		req.Header.Set("If-Modified-Since", src.LastModified)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.recordFailure(ctx, src, err)
		return
	}
	defer resp.Body.Close()

	now := time.Now().Unix()

	switch resp.StatusCode {
	case http.StatusNotModified:
		newInterval := adaptInterval(src.PollInterval, false)
		f.store.UpdatePollState(ctx, src.ID, model.PollState{
			LastPollAt: now, NextPollAt: now + newInterval,
			PollInterval: newInterval,
			ETag: src.ETag, LastModified: src.LastModified,
		})
		return
	case http.StatusNotFound, http.StatusGone:
		slog.Warn("source dead", "id", src.ID, "url", src.URL, "status", resp.StatusCode)
		f.store.UpdatePollState(ctx, src.ID, model.PollState{
			LastPollAt: now, NextPollAt: now + maxBackoff,
			PollInterval: src.PollInterval,
			ConsecFails: src.ConsecFails + 1, IsDead: true,
		})
		return
	}

	if resp.StatusCode != http.StatusOK {
		f.recordFailure(ctx, src, fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		f.recordFailure(ctx, src, err)
		return
	}

	feed, err := gofeed.NewParser().ParseString(string(body))
	if err != nil {
		f.recordFailure(ctx, src, fmt.Errorf("parse: %w", err))
		return
	}

	newCount := 0
	for _, entry := range feed.Items {
		item := entryToItem(src.ID, entry, now)
		if item == nil {
			continue
		}
		_, isNew, err := f.store.UpsertItem(ctx, item)
		if err != nil {
			slog.Error("upsert item", "source", src.ID, "err", err)
			continue
		}
		if isNew {
			newCount++
		}
	}

	newInterval := adaptInterval(src.PollInterval, newCount > 0)
	f.store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt:   now,
		NextPollAt:   now + newInterval,
		PollInterval: newInterval,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ConsecFails:  0,
		IsDead:       false,
	})
}

func entryToItem(sourceID int64, e *gofeed.Item, fetchedAt int64) *model.Item {
	raw := e.GUID
	if raw == "" {
		raw = e.Link
	}
	if raw == "" {
		return nil
	}
	h := sha256.Sum256([]byte(raw))
	guid := fmt.Sprintf("%x", h)

	link := normalize.StripTrackingParams(e.Link)

	var publishedAt int64
	switch {
	case e.PublishedParsed != nil:
		publishedAt = e.PublishedParsed.Unix()
	case e.UpdatedParsed != nil:
		publishedAt = e.UpdatedParsed.Unix()
	default:
		publishedAt = fetchedAt
	}

	content := e.Content
	if content == "" {
		content = e.Description
	}
	content = normalize.SanitizeHTML(content)

	author := ""
	if e.Author != nil {
		author = e.Author.Name
	}

	return &model.Item{
		SourceID:    sourceID,
		GUID:        guid,
		URL:         link,
		Title:       e.Title,
		Author:      author,
		PublishedAt: publishedAt,
		FetchedAt:   fetchedAt,
		ContentHTML: content,
		ContentText: normalize.HTMLToText(content),
	}
}

func (f *RSSFetcher) recordFailure(ctx context.Context, src *model.Source, err error) {
	slog.Warn("poll failure", "source", src.ID, "url", src.URL, "err", err)
	fails := src.ConsecFails + 1
	now := time.Now().Unix()
	f.store.UpdatePollState(ctx, src.ID, model.PollState{
		LastPollAt:   now,
		NextPollAt:   now + backoffSecs(fails),
		PollInterval: src.PollInterval,
		ConsecFails:  fails,
		IsDead:       fails >= maxConsecFails,
	})
}

// adaptInterval implements the ×1.5 / ÷2 adaptive logic.
func adaptInterval(current int64, hadNew bool) int64 {
	if hadNew {
		if v := current / 2; v >= minInterval {
			return v
		}
		return minInterval
	}
	if v := current * 3 / 2; v <= maxInterval {
		return v
	}
	return maxInterval
}

// backoffSecs returns exponential backoff capped at 24 h.
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
