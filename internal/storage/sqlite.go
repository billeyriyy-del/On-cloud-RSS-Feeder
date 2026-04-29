package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"rssfeeder/internal/model"

	_ "modernc.org/sqlite"
)

// sourceColumns must match the SELECT order used in scanSource.
const sourceColumns = `id, type, url, title, folder_id, scraper_config,
    next_poll_at, last_poll_at, etag, last_modified,
    consec_fails, is_dead, poll_interval, created_at, updated_at`

// Store is the single concrete storage type; exported so callers can use it directly.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at path, applies pragmas, and runs migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Single writer avoids WAL write contention.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(pragmas); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ── Sources ──────────────────────────────────────────────────────────────────

func (s *Store) CreateSource(ctx context.Context, src *model.Source) error {
	now := time.Now().Unix()
	src.CreatedAt = now
	src.UpdatedAt = now
	if src.PollInterval == 0 {
		src.PollInterval = 3600
	}

	cfgJSON, err := marshalScraperConfig(src.ScraperConfig)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sources
		    (type, url, title, folder_id, scraper_config,
		     next_poll_at, poll_interval, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		src.Type, src.URL, src.Title, src.FolderID, cfgJSON,
		src.NextPollAt, src.PollInterval, src.CreatedAt, src.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	src.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetSource(ctx context.Context, id int64) (*model.Source, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id)
	src, err := scanSource(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return src, err
}

func (s *Store) ListSources(ctx context.Context) ([]*model.Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *Store) UpdateSource(ctx context.Context, src *model.Source) error {
	src.UpdatedAt = time.Now().Unix()
	cfgJSON, err := marshalScraperConfig(src.ScraperConfig)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE sources SET
		    type=?, url=?, title=?, folder_id=?, scraper_config=?,
		    next_poll_at=?, last_poll_at=?, etag=?, last_modified=?,
		    consec_fails=?, is_dead=?, poll_interval=?, updated_at=?
		WHERE id=?`,
		src.Type, src.URL, src.Title, src.FolderID, cfgJSON,
		src.NextPollAt, src.LastPollAt, src.ETag, src.LastModified,
		src.ConsecFails, boolInt(src.IsDead), src.PollInterval, src.UpdatedAt,
		src.ID,
	)
	return err
}

func (s *Store) DeleteSource(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := deleteFTSForSource(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id); err != nil {
		return err
	}
	if err := addTombstone(ctx, tx, "source", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetDueSources(ctx context.Context, now int64) ([]*model.Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE next_poll_at<=? AND is_dead=0 ORDER BY next_poll_at`,
		now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *Store) UpdatePollState(ctx context.Context, id int64, ps model.PollState) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sources SET
		    last_poll_at=?, next_poll_at=?, poll_interval=?,
		    etag=?, last_modified=?,
		    consec_fails=?, is_dead=?, updated_at=?
		WHERE id=?`,
		ps.LastPollAt, ps.NextPollAt, ps.PollInterval,
		ps.ETag, ps.LastModified,
		ps.ConsecFails, boolInt(ps.IsDead), time.Now().Unix(),
		id,
	)
	return err
}

// ── Items ─────────────────────────────────────────────────────────────────────

// UpsertItem inserts a new item and syncs the FTS index.
// Returns (id, isNew, error). On conflict the existing row is left unchanged.
func (s *Store) UpsertItem(ctx context.Context, item *model.Item) (int64, bool, error) {
	now := time.Now().Unix()
	item.FetchedAt = now
	item.CreatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO items
		    (source_id, guid, url, title, author,
		     published_at, fetched_at, content_html, content_text, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source_id, guid) DO NOTHING`,
		item.SourceID, item.GUID, item.URL, item.Title, item.Author,
		item.PublishedAt, item.FetchedAt, item.ContentHTML, item.ContentText, item.CreatedAt,
	)
	if err != nil {
		return 0, false, fmt.Errorf("upsert item: %w", err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		var existID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM items WHERE source_id=? AND guid=?`,
			item.SourceID, item.GUID,
		).Scan(&existID); err != nil {
			return 0, false, err
		}
		tx.Commit()
		return existID, false, nil
	}

	id, _ := res.LastInsertId()
	item.ID = id

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO items_fts(rowid, title, author, content_text) VALUES (?,?,?,?)`,
		id, item.Title, item.Author, item.ContentText,
	); err != nil {
		return 0, false, fmt.Errorf("fts insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (s *Store) GetItemURLsForSource(ctx context.Context, sourceID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT url FROM items WHERE source_id=?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]bool)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		m[u] = true
	}
	return m, rows.Err()
}

func (s *Store) ListItemsSince(ctx context.Context, since int64, limit int) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_id, guid, url, title, author,
		       published_at, fetched_at, content_html, created_at
		FROM items WHERE fetched_at>? ORDER BY fetched_at LIMIT ?`,
		since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ── Item state ────────────────────────────────────────────────────────────────

func (s *Store) GetItemState(ctx context.Context, itemID int64) (*model.ItemState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT item_id, is_read, is_starred, read_at, starred_at, updated_at
		FROM item_state WHERE item_id=?`, itemID)
	st, err := scanItemState(row)
	if err == sql.ErrNoRows {
		return &model.ItemState{ItemID: itemID}, nil
	}
	return st, err
}

func (s *Store) UpsertItemState(ctx context.Context, st *model.ItemState) error {
	st.UpdatedAt = time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO item_state (item_id, is_read, is_starred, read_at, starred_at, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
		    is_read    = excluded.is_read,
		    is_starred = excluded.is_starred,
		    read_at    = excluded.read_at,
		    starred_at = excluded.starred_at,
		    updated_at = excluded.updated_at`,
		st.ItemID, boolInt(st.IsRead), boolInt(st.IsStarred),
		st.ReadAt, st.StarredAt, st.UpdatedAt,
	)
	return err
}

func (s *Store) ListChangedStatesSince(ctx context.Context, since int64) ([]*model.ItemState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT item_id, is_read, is_starred, read_at, starred_at, updated_at
		FROM item_state WHERE updated_at>? ORDER BY updated_at`,
		since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemState
	for rows.Next() {
		st, err := scanItemState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ── Folders ───────────────────────────────────────────────────────────────────

func (s *Store) CreateFolder(ctx context.Context, f *model.Folder) error {
	now := time.Now().Unix()
	f.CreatedAt, f.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (name, parent_id, created_at, updated_at) VALUES (?,?,?,?)`,
		f.Name, f.ParentID, f.CreatedAt, f.UpdatedAt,
	)
	if err != nil {
		return err
	}
	f.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetFolder(ctx context.Context, id int64) (*model.Folder, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, parent_id, created_at, updated_at FROM folders WHERE id=?`, id)
	f, err := scanFolder(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (s *Store) ListFolders(ctx context.Context) ([]*model.Folder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, parent_id, created_at, updated_at FROM folders ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) UpdateFolder(ctx context.Context, f *model.Folder) error {
	f.UpdatedAt = time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE folders SET name=?, parent_id=?, updated_at=? WHERE id=?`,
		f.Name, f.ParentID, f.UpdatedAt, f.ID,
	)
	return err
}

func (s *Store) DeleteFolder(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id=?`, id); err != nil {
		return err
	}
	if err := addTombstone(ctx, tx, "folder", id); err != nil {
		return err
	}
	return tx.Commit()
}

// FolderDepth returns how many ancestors a folder has (0 = root).
func (s *Store) FolderDepth(ctx context.Context, id int64) (int, error) {
	depth := 0
	current := id
	for {
		var parentID *int64
		err := s.db.QueryRowContext(ctx,
			`SELECT parent_id FROM folders WHERE id=?`, current,
		).Scan(&parentID)
		if err == sql.ErrNoRows || parentID == nil {
			return depth, nil
		}
		if err != nil {
			return 0, err
		}
		depth++
		current = *parentID
		if depth > 10 { // guard against accidental cycles
			return depth, nil
		}
	}
}

// ── Sync delta ────────────────────────────────────────────────────────────────

func (s *Store) GetSyncDelta(ctx context.Context, since int64) (*model.SyncDelta, error) {
	delta := &model.SyncDelta{
		Items:      []*model.Item{},
		States:     []*model.ItemState{},
		Folders:    []*model.Folder{},
		Sources:    []*model.Source{},
		Tombstones: []*model.Tombstone{},
		Cursor:     since,
	}

	items, err := s.ListItemsSince(ctx, since, 1000)
	if err != nil {
		return nil, err
	}
	delta.Items = items
	for _, it := range items {
		if it.FetchedAt > delta.Cursor {
			delta.Cursor = it.FetchedAt
		}
	}

	states, err := s.ListChangedStatesSince(ctx, since)
	if err != nil {
		return nil, err
	}
	delta.States = states
	for _, st := range states {
		if st.UpdatedAt > delta.Cursor {
			delta.Cursor = st.UpdatedAt
		}
	}

	fRows, err := s.db.QueryContext(ctx,
		`SELECT id, name, parent_id, created_at, updated_at FROM folders WHERE updated_at>? ORDER BY updated_at`,
		since)
	if err != nil {
		return nil, err
	}
	defer fRows.Close()
	for fRows.Next() {
		f, err := scanFolder(fRows)
		if err != nil {
			return nil, err
		}
		delta.Folders = append(delta.Folders, f)
		if f.UpdatedAt > delta.Cursor {
			delta.Cursor = f.UpdatedAt
		}
	}

	sRows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE updated_at>? ORDER BY updated_at`, since)
	if err != nil {
		return nil, err
	}
	defer sRows.Close()
	srcs, err := scanSources(sRows)
	if err != nil {
		return nil, err
	}
	delta.Sources = srcs
	for _, src := range srcs {
		if src.UpdatedAt > delta.Cursor {
			delta.Cursor = src.UpdatedAt
		}
	}

	tRows, err := s.db.QueryContext(ctx,
		`SELECT entity_type, entity_id, deleted_at FROM tombstones WHERE deleted_at>? ORDER BY deleted_at`,
		since)
	if err != nil {
		return nil, err
	}
	defer tRows.Close()
	for tRows.Next() {
		var t model.Tombstone
		if err := tRows.Scan(&t.EntityType, &t.EntityID, &t.DeletedAt); err != nil {
			return nil, err
		}
		delta.Tombstones = append(delta.Tombstones, &t)
		if t.DeletedAt > delta.Cursor {
			delta.Cursor = t.DeletedAt
		}
	}

	return delta, nil
}

// ── TTL sweep ─────────────────────────────────────────────────────────────────

// SweepExpiredItems deletes non-starred items older than before (unix sec).
// It manually removes them from items_fts and records tombstones.
func (s *Store) SweepExpiredItems(ctx context.Context, before int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT i.id, i.title, i.author, i.content_text
		FROM items i
		LEFT JOIN item_state st ON st.item_id = i.id
		WHERE i.fetched_at < ? AND (st.is_starred IS NULL OR st.is_starred = 0)`,
		before)
	if err != nil {
		return 0, err
	}
	type mini struct{ id int64; title, author, text string }
	var todo []mini
	for rows.Next() {
		var m mini
		if err := rows.Scan(&m.id, &m.title, &m.author, &m.text); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	for _, m := range todo {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO items_fts(items_fts, rowid, title, author, content_text) VALUES('delete',?,?,?,?)`,
			m.id, m.title, m.author, m.text,
		); err != nil {
			return 0, fmt.Errorf("fts delete %d: %w", m.id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE id=?`, m.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tombstones (entity_type, entity_id, deleted_at) VALUES ('item',?,?)`,
			m.id, now,
		); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	// Best-effort FTS optimize post-sweep.
	s.db.ExecContext(ctx, `INSERT INTO items_fts(items_fts) VALUES('optimize')`)

	return len(todo), nil
}

// SweepOldTombstones removes tombstones older than before.
func (s *Store) SweepOldTombstones(ctx context.Context, before int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tombstones WHERE deleted_at < ?`, before)
	return err
}

// VacuumDB runs VACUUM to reclaim disk space.
func (s *Store) VacuumDB(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// ── Search ────────────────────────────────────────────────────────────────────

func (s *Store) SearchItems(ctx context.Context, query string, limit int) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.source_id, i.guid, i.url, i.title, i.author,
		       i.published_at, i.fetched_at, i.content_html, i.created_at
		FROM items_fts
		JOIN items i ON i.id = items_fts.rowid
		WHERE items_fts MATCH ?
		ORDER BY rank
		LIMIT ?`,
		query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func deleteFTSForSource(ctx context.Context, tx *sql.Tx, sourceID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, title, author, content_text FROM items WHERE source_id=?`, sourceID)
	if err != nil {
		return err
	}
	type mini struct{ id int64; title, author, text string }
	var todo []mini
	for rows.Next() {
		var m mini
		if err := rows.Scan(&m.id, &m.title, &m.author, &m.text); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, m)
	}
	rows.Close()
	for _, m := range todo {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO items_fts(items_fts, rowid, title, author, content_text) VALUES('delete',?,?,?,?)`,
			m.id, m.title, m.author, m.text,
		); err != nil {
			return err
		}
	}
	return nil
}

func addTombstone(ctx context.Context, tx *sql.Tx, entityType string, id int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO tombstones (entity_type, entity_id, deleted_at) VALUES (?,?,?)`,
		entityType, id, time.Now().Unix(),
	)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func marshalScraperConfig(cfg *model.ScraperConfig) (*string, error) {
	if cfg == nil {
		return nil, nil
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// scanner unifies *sql.Row and *sql.Rows for the scan helpers.
type scanner interface {
	Scan(dest ...any) error
}

func scanSource(s scanner) (*model.Source, error) {
	var src model.Source
	var isDead int
	var cfgJSON *string
	err := s.Scan(
		&src.ID, &src.Type, &src.URL, &src.Title, &src.FolderID,
		&cfgJSON, &src.NextPollAt, &src.LastPollAt,
		&src.ETag, &src.LastModified, &src.ConsecFails, &isDead,
		&src.PollInterval, &src.CreatedAt, &src.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	src.IsDead = isDead != 0
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
	var out []*model.Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func scanItem(s scanner) (*model.Item, error) {
	var it model.Item
	err := s.Scan(
		&it.ID, &it.SourceID, &it.GUID, &it.URL,
		&it.Title, &it.Author, &it.PublishedAt,
		&it.FetchedAt, &it.ContentHTML, &it.CreatedAt,
	)
	return &it, err
}

func scanItems(rows *sql.Rows) ([]*model.Item, error) {
	var out []*model.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func scanItemState(s scanner) (*model.ItemState, error) {
	var st model.ItemState
	var isRead, isStarred int
	err := s.Scan(&st.ItemID, &isRead, &isStarred, &st.ReadAt, &st.StarredAt, &st.UpdatedAt)
	if err != nil {
		return nil, err
	}
	st.IsRead = isRead != 0
	st.IsStarred = isStarred != 0
	return &st, nil
}

func scanFolder(s scanner) (*model.Folder, error) {
	var f model.Folder
	err := s.Scan(&f.ID, &f.Name, &f.ParentID, &f.CreatedAt, &f.UpdatedAt)
	return &f, err
}
