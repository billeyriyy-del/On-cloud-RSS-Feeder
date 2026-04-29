package fetcher

import (
	"context"
	"log/slog"
	"math/rand"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
)

const workerPoolSize = 8

// Scheduler ticks every minute, dispatches due sources to a bounded worker pool,
// and runs nightly maintenance (TTL sweep, monthly VACUUM).
type Scheduler struct {
	store   *storage.Store
	rss     *RSSFetcher
	html    *HTMLScraper
	sem     chan struct{} // bounded concurrency
}

// NewScheduler constructs a Scheduler. Call Run to start it.
func NewScheduler(store *storage.Store) *Scheduler {
	return &Scheduler{
		store: store,
		rss:   newRSSFetcher(store),
		html:  newHTMLScraper(store),
		sem:   make(chan struct{}, workerPoolSize),
	}
}

// SpreadFirstPoll returns next_poll_at for a brand-new source, spread randomly
// across [now, now+interval) to prevent OPML-import thundering herd.
func SpreadFirstPoll(interval int64) int64 {
	return time.Now().Unix() + rand.Int63n(interval+1)
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	pollTicker  := time.NewTicker(time.Minute)
	sweepTicker := time.NewTicker(24 * time.Hour)
	// Monthly VACUUM: 30 days.
	vacuumTicker := time.NewTicker(30 * 24 * time.Hour)
	// Tombstone sweep: ~6 months.
	tombTicker := time.NewTicker(180 * 24 * time.Hour)

	defer pollTicker.Stop()
	defer sweepTicker.Stop()
	defer vacuumTicker.Stop()
	defer tombTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			s.tick(ctx)
		case <-sweepTicker.C:
			s.sweep(ctx)
		case <-vacuumTicker.C:
			s.vacuum(ctx)
		case <-tombTicker.C:
			s.sweepTombstones(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	sources, err := s.store.GetDueSources(ctx, time.Now().Unix())
	if err != nil {
		slog.Error("get due sources", "err", err)
		return
	}
	for _, src := range sources {
		src := src
		s.sem <- struct{}{} // acquire slot
		go func() {
			defer func() { <-s.sem }()
			s.poll(ctx, src)
		}()
	}
}

func (s *Scheduler) poll(ctx context.Context, src *model.Source) {
	switch src.Type {
	case model.SourceTypeRSS:
		s.rss.Poll(ctx, src)
	case model.SourceTypeHTML:
		s.html.Poll(ctx, src)
	default:
		slog.Warn("unknown source type", "id", src.ID, "type", src.Type)
	}
}

func (s *Scheduler) sweep(ctx context.Context) {
	cutoff := time.Now().AddDate(0, -3, 0).Unix() // 90 days ago
	n, err := s.store.SweepExpiredItems(ctx, cutoff)
	if err != nil {
		slog.Error("TTL sweep", "err", err)
		return
	}
	if n > 0 {
		slog.Info("TTL sweep complete", "deleted", n)
	}
}

func (s *Scheduler) vacuum(ctx context.Context) {
	if err := s.store.VacuumDB(ctx); err != nil {
		slog.Error("VACUUM", "err", err)
	}
}

func (s *Scheduler) sweepTombstones(ctx context.Context) {
	// Remove tombstones older than ~6 months.
	cutoff := time.Now().AddDate(0, -6, 0).Unix()
	if err := s.store.SweepOldTombstones(ctx, cutoff); err != nil {
		slog.Error("tombstone sweep", "err", err)
	}
}
