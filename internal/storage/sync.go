package storage

import (
	"context"
	"database/sql"

	"rssfeeder/internal/model"
)

// LegacySinceThreshold separates the two meanings of the old `since` parameter:
// values at or above it are unix timestamps (clients built against the
// timestamp API), values below it are change-log cursors.
const LegacySinceThreshold = 1_000_000_000

// CursorForTimestamp maps a legacy unix-seconds `since` to a cursor that is
// guaranteed not to skip anything changed at or after that second. It may
// re-send a few older rows; sync application is idempotent, so that is safe.
func (s *Store) CursorForTimestamp(ctx context.Context, since int64) (int64, error) {
	var first sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(seq) FROM change_log WHERE at >= ?`, since).Scan(&first); err != nil {
		return 0, err
	}
	if first.Valid {
		return first.Int64 - 1, nil
	}
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM change_log`).Scan(&last)
	return last, err
}

// GetSyncDelta returns up to limit changes after cursor.
//
// Paging is exact: the cursor is the change-log seq, so equal timestamps, bulk
// imports and long offline gaps cannot cause skipped or repeated pages. Each
// changed entity is returned in its *current* state (several changes to the same
// row in one page collapse to one), and all reads share one transaction so a
// page is a consistent snapshot.
func (s *Store) GetSyncDelta(ctx context.Context, cursor int64, limit int) (*model.SyncDelta, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	delta := &model.SyncDelta{
		Items:      []*model.Item{},
		States:     []*model.ItemState{},
		Folders:    []*model.Folder{},
		Sources:    []*model.Source{},
		Tombstones: []*model.Tombstone{},
		Cursor:     cursor,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var floor int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='sync_floor'`).Scan(&floor); err != nil {
		return nil, err
	}
	if cursor > 0 && cursor < floor {
		delta.Reset = true
		delta.Cursor = 0
		delta.HasMore = true
		return delta, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT seq, entity, entity_id, op, at FROM change_log WHERE seq > ? ORDER BY seq LIMIT ?`,
		cursor, limit+1)
	if err != nil {
		return nil, err
	}
	type change struct {
		seq, id, at int64
		entity, op  string
	}
	var changes []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.seq, &c.entity, &c.id, &c.op, &c.at); err != nil {
			rows.Close()
			return nil, err
		}
		changes = append(changes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(changes) > limit {
		changes = changes[:limit]
		delta.HasMore = true
	}
	if len(changes) == 0 {
		return delta, nil
	}
	delta.Cursor = changes[len(changes)-1].seq

	// Collapse to the last change per entity, keeping log order.
	type key struct {
		entity string
		id     int64
	}
	last := make(map[key]int, len(changes))
	for i, c := range changes {
		last[key{c.entity, c.id}] = i
	}
	upserts := map[string][]int64{}
	for i, c := range changes {
		if last[key{c.entity, c.id}] != i {
			continue
		}
		if c.op == "delete" {
			if c.entity != "state" {
				delta.Tombstones = append(delta.Tombstones, &model.Tombstone{EntityType: c.entity, EntityID: c.id, DeletedAt: c.at})
			}
			continue
		}
		upserts[c.entity] = append(upserts[c.entity], c.id)
	}

	// Fetch current rows. An upsert whose row is gone is skipped: its delete is
	// later in the log and will arrive on a later page.
	if ids := upserts["folder"]; len(ids) > 0 {
		if err := forChunks(ctx, tx, ids, `SELECT `+folderColumns+` FROM folders WHERE id IN (%s)`, func(r *sql.Rows) error {
			f, err := scanFolder(r)
			if err == nil {
				delta.Folders = append(delta.Folders, f)
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	if ids := upserts["source"]; len(ids) > 0 {
		if err := forChunks(ctx, tx, ids, `SELECT `+sourceColumns+` FROM sources WHERE id IN (%s)`, func(r *sql.Rows) error {
			src, err := scanSource(r)
			if err == nil {
				delta.Sources = append(delta.Sources, src)
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	if ids := upserts["item"]; len(ids) > 0 {
		items, err := getItemsByID(ctx, tx, ids)
		if err != nil {
			return nil, err
		}
		delta.Items = items
	}
	if ids := upserts["state"]; len(ids) > 0 {
		if err := forChunks(ctx, tx, ids, `SELECT `+stateColumns+` FROM item_state WHERE item_id IN (%s)`, func(r *sql.Rows) error {
			st, err := scanItemState(r)
			if err == nil {
				delta.States = append(delta.States, st)
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	return delta, tx.Commit()
}

func forChunks(ctx context.Context, x execer, ids []int64, query string, fn func(*sql.Rows) error) error {
	for _, chunk := range chunks(ids, 500) {
		ph, args := placeholders(chunk)
		rows, err := x.QueryContext(ctx, sprintfIn(query, ph), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := fn(rows); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// sprintfIn substitutes the single %s in query (avoids fmt for SQL with % literals).
func sprintfIn(query, ph string) string {
	for i := 0; i+1 < len(query); i++ {
		if query[i] == '%' && query[i+1] == 's' {
			return query[:i] + ph + query[i+2:]
		}
	}
	return query
}
