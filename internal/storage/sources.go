package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"rssfeeder/internal/model"
)

// ErrDuplicate is returned when a unique constraint (e.g. source URL) is violated.
var ErrDuplicate = errors.New("duplicate")

// sourceColumns must match the SELECT order used in scanSource.
const sourceColumns = `id, type, url, title, site_url, icon_url, description, kind,
    folder_id, scraper_config, fetch_full_text,
    next_poll_at, last_poll_at, etag, last_modified,
    consec_fails, last_error, is_dead, poll_interval,
    websub_hub, websub_topic, websub_secret, websub_expires_at,
    created_at, updated_at`

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) CreateSource(ctx context.Context, src *model.Source) error {
	now := s.now()
	src.CreatedAt, src.UpdatedAt = now, now
	if src.PollInterval == 0 {
		src.PollInterval = 3600
	}
	if src.Kind == "" {
		src.Kind = model.KindArticle
	}
	cfgJSON, err := marshalScraperConfig(src.ScraperConfig)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sources
		    (type, url, title, site_url, icon_url, description, kind, folder_id, scraper_config,
		     fetch_full_text, next_poll_at, poll_interval, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		src.Type, src.URL, src.Title, src.SiteURL, src.IconURL, src.Description, src.Kind,
		src.FolderID, cfgJSON, boolInt(src.FetchFullText),
		src.NextPollAt, src.PollInterval, src.CreatedAt, src.UpdatedAt,
	)
	if isUniqueErr(err) {
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	src.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetSource(ctx context.Context, id int64) (*model.Source, error) {
	src, err := scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return src, err
}

// GetSourceByURL finds a source by its feed URL (exact match).
func (s *Store) GetSourceByURL(ctx context.Context, u string) (*model.Source, error) {
	src, err := scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE url = ?`, u))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return src, err
}

func (s *Store) ListSources(ctx context.Context) ([]*model.Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

// UpdateSource writes the user-editable fields only; poll bookkeeping belongs
// to the scheduler (UpdatePollState), so a PUT cannot clobber a poll that
// finished meanwhile. Changing the URL starts the feed afresh: validators,
// failure count and push subscription belong to the old address.
func (s *Store) UpdateSource(ctx context.Context, src *model.Source) error {
	src.UpdatedAt = s.now()
	cfgJSON, err := marshalScraperConfig(src.ScraperConfig)
	if err != nil {
		return err
	}
	// In SQLite every SET expression sees the row as it was before the update,
	// so "url <> ?" compares against the old URL.
	_, err = s.db.ExecContext(ctx, `
		UPDATE sources SET
		    etag          = CASE WHEN url <> ?1 THEN '' ELSE etag END,
		    last_modified = CASE WHEN url <> ?1 THEN '' ELSE last_modified END,
		    consec_fails  = CASE WHEN url <> ?1 THEN 0 ELSE consec_fails END,
		    is_dead       = CASE WHEN url <> ?1 THEN 0 ELSE is_dead END,
		    next_poll_at  = CASE WHEN url <> ?1 THEN ?2 ELSE next_poll_at END,
		    websub_hub    = CASE WHEN url <> ?1 THEN '' ELSE websub_hub END,
		    websub_topic  = CASE WHEN url <> ?1 THEN '' ELSE websub_topic END,
		    websub_expires_at = CASE WHEN url <> ?1 THEN 0 ELSE websub_expires_at END,
		    url=?1, type=?3, title=?4, folder_id=?5, scraper_config=?6, fetch_full_text=?7,
		    poll_interval=?8, updated_at=?2
		WHERE id=?9`,
		src.URL, src.UpdatedAt, src.Type, src.Title, src.FolderID, cfgJSON, boolInt(src.FetchFullText),
		src.PollInterval, src.ID,
	)
	if isUniqueErr(err) {
		return ErrDuplicate
	}
	return err
}

// UpdateFeedMeta refreshes display metadata learned from the feed itself.
// Title is only filled when the user has not set one (empty or still the URL).
func (s *Store) UpdateFeedMeta(ctx context.Context, id int64, m model.FeedMeta) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sources SET
		    title       = CASE WHEN (title = '' OR title = url) AND ? <> '' THEN ? ELSE title END,
		    site_url    = CASE WHEN ? <> '' THEN ? ELSE site_url END,
		    icon_url    = CASE WHEN ? <> '' THEN ? ELSE icon_url END,
		    description = CASE WHEN ? <> '' THEN ? ELSE description END,
		    kind        = CASE WHEN ? <> '' THEN ? ELSE kind END
		WHERE id = ?`,
		m.Title, m.Title, m.SiteURL, m.SiteURL, m.IconURL, m.IconURL,
		m.Description, m.Description, string(m.Kind), string(m.Kind), id)
	return err
}

// MoveSourceURL records a permanent redirect. It is a no-op if another source
// already owns the target URL (the user subscribed twice; keep both rows).
func (s *Store) MoveSourceURL(ctx context.Context, id int64, newURL string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sources SET url=?, updated_at=? WHERE id=? AND NOT EXISTS (SELECT 1 FROM sources WHERE url=?)`,
		newURL, s.now(), id, newURL)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReviveSource brings a dead feed back into the polling rotation now.
func (s *Store) ReviveSource(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sources SET is_dead=0, consec_fails=0, last_error='', next_poll_at=? WHERE id=?`, s.now(), id)
	return err
}

func (s *Store) DeleteSource(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := ftsDeleteWhere(ctx, tx, `source_id=?`, id); err != nil {
		return err
	}
	// Cascades to items, item_state, enclosures and full text; triggers log deletes.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// GetDueSources returns live sources whose next poll is due, oldest first.
func (s *Store) GetDueSources(ctx context.Context, now int64) ([]*model.Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE next_poll_at<=? AND is_dead=0 ORDER BY next_poll_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

// RequestPoll makes sources due now (pull-to-refresh). Sources polled within
// minGap seconds are left alone so a refresh storm cannot hammer publishers.
// sourceID 0 means every live source. Returns how many were queued.
func (s *Store) RequestPoll(ctx context.Context, sourceID int64, minGap int64) (int64, error) {
	now := s.now()
	q := `UPDATE sources SET next_poll_at=? WHERE is_dead=0 AND next_poll_at>? AND COALESCE(last_poll_at,0) <= ?`
	args := []any{now, now, now - minGap}
	if sourceID > 0 {
		q += ` AND id=?`
		args = append(args, sourceID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) UpdatePollState(ctx context.Context, id int64, ps model.PollState) error {
	if len(ps.LastError) > 500 {
		ps.LastError = ps.LastError[:500]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE sources SET
		    last_poll_at=?, next_poll_at=?, poll_interval=?,
		    etag=?, last_modified=?,
		    consec_fails=?, last_error=?, is_dead=?
		WHERE id=?`,
		ps.LastPollAt, ps.NextPollAt, ps.PollInterval,
		ps.ETag, ps.LastModified,
		ps.ConsecFails, ps.LastError, boolInt(ps.IsDead),
		id,
	)
	return err
}

// SetWebSub stores (or clears, with a zero value) a source's push subscription.
func (s *Store) SetWebSub(ctx context.Context, id int64, w model.WebSub) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sources SET websub_hub=?, websub_topic=?, websub_secret=?, websub_expires_at=? WHERE id=?`,
		w.Hub, w.Topic, w.Secret, w.ExpiresAt, id)
	return err
}

// ListWebSubRenewals returns push sources whose lease ends before `before`,
// plus sources that know a hub but have no active lease.
func (s *Store) ListWebSubRenewals(ctx context.Context, before int64) ([]*model.Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources
		WHERE websub_hub <> '' AND is_dead = 0 AND websub_expires_at < ?`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func scanSource(sc scanner) (*model.Source, error) {
	var src model.Source
	var isDead, fullText int
	var cfgJSON *string
	var kind string
	err := sc.Scan(
		&src.ID, &src.Type, &src.URL, &src.Title, &src.SiteURL, &src.IconURL, &src.Description, &kind,
		&src.FolderID, &cfgJSON, &fullText,
		&src.NextPollAt, &src.LastPollAt, &src.ETag, &src.LastModified,
		&src.ConsecFails, &src.LastError, &isDead, &src.PollInterval,
		&src.WebSub.Hub, &src.WebSub.Topic, &src.WebSub.Secret, &src.WebSub.ExpiresAt,
		&src.CreatedAt, &src.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	src.Kind = model.ItemKind(kind)
	src.IsDead = isDead != 0
	src.FetchFullText = fullText != 0
	src.Push = src.WebSub.ExpiresAt > time.Now().Unix()
	if cfgJSON != nil {
		var cfg model.ScraperConfig
		if err := json.Unmarshal([]byte(*cfgJSON), &cfg); err != nil {
			return nil, fmt.Errorf("unmarshal scraper_config: %w", err)
		}
		src.ScraperConfig = &cfg
	}
	return &src, nil
}

func scanSources(rows *sql.Rows) ([]*model.Source, error) {
	out := []*model.Source{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}
