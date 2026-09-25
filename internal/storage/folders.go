package storage

import (
	"context"
	"database/sql"

	"rssfeeder/internal/model"
)

const folderColumns = `id, name, parent_id, created_at, updated_at`

func (s *Store) CreateFolder(ctx context.Context, f *model.Folder) error {
	now := s.now()
	f.CreatedAt, f.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (name, parent_id, created_at, updated_at) VALUES (?,?,?,?)`,
		f.Name, f.ParentID, f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return err
	}
	f.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetFolder(ctx context.Context, id int64) (*model.Folder, error) {
	f, err := scanFolder(s.db.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM folders WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (s *Store) ListFolders(ctx context.Context) ([]*model.Folder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+folderColumns+` FROM folders ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Folder{}
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
	f.UpdatedAt = s.now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE folders SET name=?, parent_id=?, updated_at=? WHERE id=?`,
		f.Name, f.ParentID, f.UpdatedAt, f.ID)
	return err
}

// DeleteFolder removes a folder. Its sources and subfolders move to the root via
// ON DELETE SET NULL; bumping updated_at here keeps REST readers consistent, and
// the triggers log each released row for sync.
func (s *Store) DeleteFolder(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE sources SET updated_at=? WHERE folder_id=?`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE folders SET updated_at=? WHERE parent_id=?`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id=?`, id); err != nil {
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
		err := s.db.QueryRowContext(ctx, `SELECT parent_id FROM folders WHERE id=?`, current).Scan(&parentID)
		if err == sql.ErrNoRows || (err == nil && parentID == nil) {
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

// IsDescendant reports whether candidate is folder id itself or lies beneath it
// (used to reject reparenting a folder into its own subtree).
func (s *Store) IsDescendant(ctx context.Context, id, candidate int64) (bool, error) {
	current := candidate
	for i := 0; i < 16; i++ {
		if current == id {
			return true, nil
		}
		var parentID *int64
		err := s.db.QueryRowContext(ctx, `SELECT parent_id FROM folders WHERE id=?`, current).Scan(&parentID)
		if err == sql.ErrNoRows || (err == nil && parentID == nil) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		current = *parentID
	}
	return true, nil
}

func scanFolder(sc scanner) (*model.Folder, error) {
	var f model.Folder
	err := sc.Scan(&f.ID, &f.Name, &f.ParentID, &f.CreatedAt, &f.UpdatedAt)
	return &f, err
}

// SubtreeHeight returns how many levels of folders sit below id (0 = leaf).
func (s *Store) SubtreeHeight(ctx context.Context, id int64) (int, error) {
	var h int
	err := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE sub(id, d) AS (
			SELECT ?, 0
			UNION ALL SELECT f.id, sub.d + 1 FROM folders f JOIN sub ON f.parent_id = sub.id WHERE sub.d < 10)
		SELECT MAX(d) FROM sub`, id).Scan(&h)
	return h, err
}
