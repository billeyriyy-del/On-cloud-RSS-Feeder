package storage

import (
	"context"
	"database/sql"

	"rssfeeder/internal/model"
)

const stateColumns = `item_id, is_read, is_starred, read_at, starred_at, read_changed_at, star_changed_at, updated_at`

// maxClockSkew bounds how far in the future a client timestamp may be. A device
// with a fast clock could otherwise make its writes unbeatable for hours.
const maxClockSkew = 300

func (s *Store) GetItemState(ctx context.Context, itemID int64) (*model.ItemState, error) {
	return getItemState(ctx, s.db, itemID)
}

func getItemState(ctx context.Context, x execer, itemID int64) (*model.ItemState, error) {
	st, err := scanItemState(x.QueryRowContext(ctx, `SELECT `+stateColumns+` FROM item_state WHERE item_id=?`, itemID))
	if err == sql.ErrNoRows {
		return &model.ItemState{ItemID: itemID}, nil
	}
	return st, err
}

// ApplyStateOps applies client state changes with per-field last-write-wins on
// the client's timestamp. Ops for unknown items are skipped (the item may have
// been swept while the client was offline). Returns the resulting states.
func (s *Store) ApplyStateOps(ctx context.Context, ops []model.StateOp) ([]*model.ItemState, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	out := make([]*model.ItemState, 0, len(ops))
	seen := map[int64]*model.ItemState{}
	for _, op := range ops {
		at := op.At
		if at <= 0 || at > now+maxClockSkew {
			at = now
		}
		st := seen[op.ItemID]
		if st == nil {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE id=?`, op.ItemID).Scan(&exists); err != nil {
				return nil, err
			}
			if exists == 0 {
				continue
			}
			if st, err = getItemState(ctx, tx, op.ItemID); err != nil {
				return nil, err
			}
			seen[op.ItemID] = st
			out = append(out, st)
		}
		changed := false
		if op.IsRead != nil && at >= st.ReadChangedAt {
			st.IsRead, st.ReadChangedAt, changed = *op.IsRead, at, true
			if st.IsRead {
				t := at
				st.ReadAt = &t
			} else {
				st.ReadAt = nil
			}
		}
		if op.IsStarred != nil && at >= st.StarChangedAt {
			st.IsStarred, st.StarChangedAt, changed = *op.IsStarred, at, true
			if st.IsStarred {
				t := at
				st.StarredAt = &t
			} else {
				st.StarredAt = nil
			}
		}
		if !changed {
			continue
		}
		st.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO item_state (`+stateColumns+`) VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(item_id) DO UPDATE SET
			    is_read=excluded.is_read, is_starred=excluded.is_starred,
			    read_at=excluded.read_at, starred_at=excluded.starred_at,
			    read_changed_at=excluded.read_changed_at, star_changed_at=excluded.star_changed_at,
			    updated_at=excluded.updated_at`,
			st.ItemID, boolInt(st.IsRead), boolInt(st.IsStarred), st.ReadAt, st.StarredAt,
			st.ReadChangedAt, st.StarChangedAt, st.UpdatedAt); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// MarkScope selects what "mark all as read" covers.
type MarkScope struct {
	SourceID int64
	FolderID int64
	MaxID    int64 // only items the client had seen (id <= MaxID); 0 = all
	Before   int64 // only items published before this time; 0 = any
	Read     bool  // true: mark read; false: mark unread (undo)
	IDs      []int64
	At       int64
}

// MarkRead flips is_read for every item in scope in one statement (LWW-guarded)
// and returns the ids it changed, so a client can offer undo.
func (s *Store) MarkRead(ctx context.Context, m MarkScope) ([]int64, error) {
	now := s.now()
	at := m.At
	if at <= 0 || at > now+maxClockSkew {
		at = now
	}
	var readAt any
	if m.Read {
		readAt = at
	}
	conds := `COALESCE(st.is_read, 0) = ?`
	// Bind order: SELECT list (is_read, read_at, read_changed_at, updated_at), then WHERE.
	args := []any{boolInt(m.Read), readAt, at, now, boolInt(!m.Read)}
	if m.SourceID > 0 {
		conds += ` AND i.source_id = ?`
		args = append(args, m.SourceID)
	}
	if m.FolderID > 0 {
		conds += ` AND i.source_id IN (` + folderSourcesSQL + `)`
		args = append(args, m.FolderID)
	}
	if m.MaxID > 0 {
		conds += ` AND i.id <= ?`
		args = append(args, m.MaxID)
	}
	if m.Before > 0 {
		conds += ` AND i.published_at < ?`
		args = append(args, m.Before)
	}
	if len(m.IDs) > 0 {
		if len(m.IDs) > 5000 {
			m.IDs = m.IDs[:5000]
		}
		ph, idArgs := placeholders(m.IDs)
		conds += ` AND i.id IN (` + ph + `)`
		args = append(args, idArgs...)
	}
	rows, err := s.db.QueryContext(ctx, `
		INSERT INTO item_state (item_id, is_read, is_starred, read_at, read_changed_at, star_changed_at, updated_at)
		SELECT i.id, ?, COALESCE(st.is_starred, 0), ?, ?, COALESCE(st.star_changed_at, 0), ?
		FROM items i LEFT JOIN item_state st ON st.item_id = i.id
		WHERE `+conds+`
		ON CONFLICT(item_id) DO UPDATE SET
		    is_read = excluded.is_read, read_at = excluded.read_at,
		    read_changed_at = excluded.read_changed_at, updated_at = excluded.updated_at
		WHERE item_state.read_changed_at <= excluded.read_changed_at
		RETURNING item_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanItemState(sc scanner) (*model.ItemState, error) {
	var st model.ItemState
	var isRead, isStarred int
	err := sc.Scan(&st.ItemID, &isRead, &isStarred, &st.ReadAt, &st.StarredAt,
		&st.ReadChangedAt, &st.StarChangedAt, &st.UpdatedAt)
	if err != nil {
		return nil, err
	}
	st.IsRead, st.IsStarred = isRead != 0, isStarred != 0
	return &st, nil
}
