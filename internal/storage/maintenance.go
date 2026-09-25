package storage

import (
	"context"
)

// SweepExpiredItems deletes non-starred items fetched before `before` (unix
// seconds) — the server's hard retention cutoff. Relevance decay is a client
// concern; this only bounds disk. Deletes are logged for sync by trigger.
func (s *Store) SweepExpiredItems(ctx context.Context, before int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	const where = `fetched_at < ? AND id NOT IN (SELECT item_id FROM item_state WHERE is_starred = 1)`
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO swept_guids (source_id, guid) SELECT source_id, guid FROM items WHERE `+where, before); err != nil {
		return 0, err
	}
	ids, err := ftsDeleteWhere(ctx, tx, where, before)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE `+where, before); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		s.db.ExecContext(ctx, `INSERT INTO items_fts(items_fts) VALUES('optimize')`) // best effort
	}
	return len(ids), nil
}

// CompactChangeLog keeps the log small without breaking any client:
//  1. only the newest entry per entity is kept (older ones are redundant because
//     sync always sends current state);
//  2. state entries for items that no longer exist are dropped;
//  3. delete entries older than `deletesBefore` are dropped, and sync_floor is
//     raised past them — a client whose cursor is below the floor is told to
//     reset instead of silently missing those deletions.
func (s *Store) CompactChangeLog(ctx context.Context, deletesBefore int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var removed int64
	for _, q := range []string{
		`DELETE FROM change_log WHERE seq NOT IN (SELECT MAX(seq) FROM change_log GROUP BY entity, entity_id)`,
		`DELETE FROM change_log WHERE entity = 'state' AND entity_id NOT IN (SELECT item_id FROM item_state)`,
	} {
		res, err := tx.ExecContext(ctx, q)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	var maxDel int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM change_log WHERE op = 'delete' AND at < ?`, deletesBefore).Scan(&maxDel); err != nil {
		return 0, err
	}
	if maxDel > 0 {
		res, err := tx.ExecContext(ctx, `DELETE FROM change_log WHERE op = 'delete' AND seq <= ?`, maxDel)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed += n
		if _, err := tx.ExecContext(ctx,
			`UPDATE meta SET value = MAX(value, ?) WHERE key = 'sync_floor'`, maxDel); err != nil {
			return 0, err
		}
	}
	return removed, tx.Commit()
}

// VacuumDB runs VACUUM to reclaim disk space.
func (s *Store) VacuumDB(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// Backup writes a consistent online copy of the database to path (VACUUM INTO),
// for the daily local backup the ops plan calls for.
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
