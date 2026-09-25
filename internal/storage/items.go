package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"rssfeeder/internal/model"
)

// itemSelect returns the SELECT list + FROM for items. Content is optional so
// timelines stay small; when full text was extracted it replaces feed content.
func itemSelect(withContent, withView bool) string {
	content := `''`
	if withContent {
		content = `CASE WHEN f.status='ok' THEN f.content_html ELSE i.content_html END`
	}
	q := `SELECT i.id, i.source_id, i.guid, i.url, i.title, i.author, i.summary, i.image_url, i.kind, i.lang,
	       i.reading_secs, i.published_at, i.fetched_at, i.updated_at, i.created_at,
	       ` + content + `, COALESCE(f.status = 'ok', 0)`
	if withView {
		q += `, COALESCE(st.is_read, 0), COALESCE(st.is_starred, 0), src.title`
	}
	q += `
	FROM items i
	LEFT JOIN item_fulltext f ON f.item_id = i.id`
	if withView {
		q += `
	LEFT JOIN item_state st ON st.item_id = i.id
	JOIN sources src ON src.id = i.source_id`
	}
	return q
}

func scanItemRow(sc scanner, withView bool) (*model.ItemView, error) {
	it := &model.Item{}
	v := &model.ItemView{Item: it}
	var kind string
	var full int
	dest := []any{&it.ID, &it.SourceID, &it.GUID, &it.URL, &it.Title, &it.Author, &it.Summary, &it.ImageURL,
		&kind, &it.Lang, &it.ReadingSecs, &it.PublishedAt, &it.FetchedAt, &it.UpdatedAt, &it.CreatedAt,
		&it.ContentHTML, &full}
	var isRead, isStarred int
	if withView {
		dest = append(dest, &isRead, &isStarred, &v.SourceTitle)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	it.Kind = model.ItemKind(kind)
	it.HasFullText = full != 0
	v.IsRead, v.IsStarred = isRead != 0, isStarred != 0
	return v, nil
}

// UpsertItem inserts a new item (with enclosures) and indexes it for search.
// An existing item is rewritten only when the feed says the entry was updated
// after we stored it; otherwise it is left alone. Returns (id, isNew, error).
func (s *Store) UpsertItem(ctx context.Context, item *model.Item) (int64, bool, error) {
	now := s.now()
	if item.FetchedAt == 0 {
		item.FetchedAt = now
	}
	item.CreatedAt, item.UpdatedAt = now, now
	if item.Kind == "" {
		item.Kind = model.KindArticle
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var (
		existID                      int64
		oldTitle, oldAuthor, oldText string
		oldEntryUpd                  int64
		hasFull                      int
	)
	err = tx.QueryRowContext(ctx, `
		SELECT i.id, i.title, i.author, i.content_text, i.entry_updated_at, COALESCE(f.status='ok',0)
		FROM items i LEFT JOIN item_fulltext f ON f.item_id = i.id
		WHERE i.source_id=? AND i.guid=?`, item.SourceID, item.GUID,
	).Scan(&existID, &oldTitle, &oldAuthor, &oldText, &oldEntryUpd, &hasFull)

	switch {
	case err == sql.ErrNoRows:
		res, err := tx.ExecContext(ctx, `
			INSERT INTO items
			    (source_id, guid, url, title, author, summary, image_url, kind, lang, reading_secs,
			     published_at, fetched_at, entry_updated_at, content_html, content_text, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.SourceID, item.GUID, item.URL, item.Title, item.Author, item.Summary, item.ImageURL,
			string(item.Kind), item.Lang, item.ReadingSecs,
			item.PublishedAt, item.FetchedAt, item.EntryUpdAt, item.ContentHTML, item.ContentText,
			item.CreatedAt, item.UpdatedAt,
		)
		if err != nil {
			return 0, false, fmt.Errorf("insert item: %w", err)
		}
		item.ID, _ = res.LastInsertId()
		if err := ftsInsert(ctx, tx, item.ID, item.Title, item.Author, item.ContentText); err != nil {
			return 0, false, fmt.Errorf("fts insert: %w", err)
		}
		if err := writeEnclosures(ctx, tx, item.ID, item.Enclosures); err != nil {
			return 0, false, err
		}
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return item.ID, true, nil

	case err != nil:
		return 0, false, err
	}

	item.ID = existID
	switch {
	case item.EntryUpdAt > 0 && oldEntryUpd == 0:
		// First sighting of an <updated> date for an item stored before we tracked
		// it: remember it without rewriting (no client-visible change, no sync churn).
		if _, err := tx.ExecContext(ctx, `UPDATE items SET entry_updated_at=? WHERE id=?`, item.EntryUpdAt, existID); err != nil {
			return 0, false, err
		}
	case item.EntryUpdAt > oldEntryUpd && oldEntryUpd > 0:
		// The publisher edited the entry: take the new content.
		text := item.ContentText
		if hasFull != 0 {
			text = oldText // keep the full-text index; only title/author change
		}
		if err := ftsDelete(ctx, tx, existID, oldTitle, oldAuthor, oldText); err != nil {
			return 0, false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE items SET url=?, title=?, author=?, summary=?, image_url=?, kind=?, reading_secs=?,
			    content_html=?, content_text=?, entry_updated_at=?, updated_at=?
			WHERE id=?`,
			item.URL, item.Title, item.Author, item.Summary, item.ImageURL, string(item.Kind), item.ReadingSecs,
			item.ContentHTML, text, item.EntryUpdAt, now, existID); err != nil {
			return 0, false, err
		}
		if err := ftsInsert(ctx, tx, existID, item.Title, item.Author, text); err != nil {
			return 0, false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_enclosures WHERE item_id=?`, existID); err != nil {
			return 0, false, err
		}
		if err := writeEnclosures(ctx, tx, existID, item.Enclosures); err != nil {
			return 0, false, err
		}
	default:
		return existID, false, nil
	}
	return existID, false, tx.Commit()
}

func writeEnclosures(ctx context.Context, tx *sql.Tx, itemID int64, encs []*model.Enclosure) error {
	for _, e := range encs {
		if e == nil || e.URL == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO item_enclosures (item_id, url, type, length, duration) VALUES (?,?,?,?,?)
			ON CONFLICT(item_id, url) DO NOTHING`,
			itemID, e.URL, e.Type, e.Length, e.Duration); err != nil {
			return fmt.Errorf("enclosure: %w", err)
		}
	}
	return nil
}

// attachEnclosures loads enclosures for the given items in one query per chunk.
func attachEnclosures(ctx context.Context, x execer, items []*model.Item) error {
	if len(items) == 0 {
		return nil
	}
	byID := make(map[int64]*model.Item, len(items))
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		byID[it.ID] = it
		ids = append(ids, it.ID)
	}
	for _, chunk := range chunks(ids, 500) {
		ph, args := placeholders(chunk)
		rows, err := x.QueryContext(ctx,
			`SELECT item_id, url, type, length, duration FROM item_enclosures WHERE item_id IN (`+ph+`) ORDER BY rowid`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			e := &model.Enclosure{}
			if err := rows.Scan(&id, &e.URL, &e.Type, &e.Length, &e.Duration); err != nil {
				rows.Close()
				return err
			}
			if it := byID[id]; it != nil {
				it.Enclosures = append(it.Enclosures, e)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// getItemsByID fetches full items (with content and enclosures) for sync.
func getItemsByID(ctx context.Context, x execer, ids []int64) ([]*model.Item, error) {
	out := make([]*model.Item, 0, len(ids))
	for _, chunk := range chunks(ids, 500) {
		ph, args := placeholders(chunk)
		rows, err := x.QueryContext(ctx, itemSelect(true, false)+` WHERE i.id IN (`+ph+`) ORDER BY i.id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			v, err := scanItemRow(rows, false)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, v.Item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, attachEnclosures(ctx, x, out)
}

// GetItem returns one item with state; original=true skips extracted full text.
func (s *Store) GetItem(ctx context.Context, id int64, original bool) (*model.ItemView, error) {
	q := itemSelect(true, true) + ` WHERE i.id = ?`
	if original {
		q = strings.Replace(q, `CASE WHEN f.status='ok' THEN f.content_html ELSE i.content_html END`, `i.content_html`, 1)
	}
	v, err := scanItemRow(s.db.QueryRowContext(ctx, q, id), true)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, attachEnclosures(ctx, s.db, []*model.Item{v.Item})
}

// GetItemText returns the indexed plain text (full text when extracted) for speech.
func (s *Store) GetItemText(ctx context.Context, id int64) (string, error) {
	var t string
	err := s.db.QueryRowContext(ctx, `SELECT content_text FROM items WHERE id=?`, id).Scan(&t)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return t, err
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

// ── Timeline ──────────────────────────────────────────────────────────────────

// ItemQuery filters a timeline. Zero values mean "no filter".
type ItemQuery struct {
	SourceID    int64
	FolderID    int64 // includes subfolders
	Unread      bool
	Starred     bool
	Kind        model.ItemKind
	Since       int64 // published_at >= Since
	MaxID       int64 // id <= MaxID (pin a list against items arriving mid-session)
	Oldest      bool  // oldest first
	Q           string
	WithContent bool
	Limit       int
	Offset      int // search only (ranked results have no stable keyset)

	// Keyset cursor: the last row of the previous page.
	AfterPub int64
	AfterID  int64
}

// folderSourcesSQL selects the source ids in a folder and all its descendants.
const folderSourcesSQL = `SELECT id FROM sources WHERE folder_id IN (
	WITH RECURSIVE sub(id) AS (SELECT ? UNION ALL SELECT folders.id FROM folders JOIN sub ON folders.parent_id = sub.id)
	SELECT id FROM sub)`

func (q ItemQuery) where() (string, []any) {
	var conds []string
	var args []any
	if q.SourceID > 0 {
		conds = append(conds, `i.source_id = ?`)
		args = append(args, q.SourceID)
	}
	if q.FolderID > 0 {
		conds = append(conds, `i.source_id IN (`+folderSourcesSQL+`)`)
		args = append(args, q.FolderID)
	}
	if q.Unread {
		conds = append(conds, `COALESCE(st.is_read, 0) = 0`)
	}
	if q.Starred {
		conds = append(conds, `st.is_starred = 1`)
	}
	if q.Kind != "" {
		conds = append(conds, `i.kind = ?`)
		args = append(args, string(q.Kind))
	}
	if q.Since > 0 {
		conds = append(conds, `i.published_at >= ?`)
		args = append(args, q.Since)
	}
	if q.MaxID > 0 {
		conds = append(conds, `i.id <= ?`)
		args = append(args, q.MaxID)
	}
	return strings.Join(conds, " AND "), args
}

// ListItems returns a page of the timeline (newest first unless Oldest).
func (s *Store) ListItems(ctx context.Context, q ItemQuery) ([]*model.ItemView, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	where, args := q.where()
	sqlq := itemSelect(q.WithContent, true)
	var order string
	if q.Q != "" {
		match := FTSQuery(q.Q)
		if match == "" {
			return []*model.ItemView{}, nil
		}
		sqlq += ` JOIN items_fts ON items_fts.rowid = i.id`
		where = joinConds(where, `items_fts MATCH ?`)
		args = append(args, match)
		order = ` ORDER BY items_fts.rank LIMIT ? OFFSET ?`
		args = append(args, q.Limit, q.Offset)
	} else {
		cmp, dir := "<", "DESC"
		if q.Oldest {
			cmp, dir = ">", "ASC"
		}
		if q.AfterID > 0 {
			where = joinConds(where, `(i.published_at `+cmp+` ? OR (i.published_at = ? AND i.id `+cmp+` ?))`)
			args = append(args, q.AfterPub, q.AfterPub, q.AfterID)
		}
		order = ` ORDER BY i.published_at ` + dir + `, i.id ` + dir + ` LIMIT ?`
		args = append(args, q.Limit)
	}
	if where != "" {
		sqlq += ` WHERE ` + where
	}
	rows, err := s.db.QueryContext(ctx, sqlq+order, args...)
	if err != nil {
		return nil, err
	}
	out := []*model.ItemView{}
	items := []*model.Item{}
	for rows.Next() {
		v, err := scanItemRow(rows, true)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
		items = append(items, v.Item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, attachEnclosures(ctx, s.db, items)
}

func joinConds(a, b string) string {
	if a == "" {
		return b
	}
	return a + " AND " + b
}

// SearchItems is kept for the /items/search endpoint (ranked, with content).
func (s *Store) SearchItems(ctx context.Context, query string, limit int) ([]*model.ItemView, error) {
	return s.ListItems(ctx, ItemQuery{Q: query, Limit: limit, WithContent: true})
}

// FTSQuery turns free text into a safe FTS5 query: every term is quoted (so
// "c++", "don't" or a stray quote can't raise a syntax error) and the last one
// is a prefix match, which is what search-as-you-type needs.
func FTSQuery(in string) string {
	fields := strings.FieldsFunc(in, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"'
	})
	var terms []string
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
		if f == "" {
			continue
		}
		terms = append(terms, `"`+f+`"`)
	}
	if len(terms) == 0 {
		return ""
	}
	terms[len(terms)-1] += "*"
	return strings.Join(terms, " ")
}

// ── Counts ────────────────────────────────────────────────────────────────────

// Counts summarises unread items per source (for sidebars, badges and voice).
type Counts struct {
	Unread   int64           `json:"unread"`
	Starred  int64           `json:"starred"`
	BySource map[int64]int64 `json:"by_source"`
	MaxID    int64           `json:"max_id"`
}

func (s *Store) GetCounts(ctx context.Context) (*Counts, error) {
	c := &Counts{BySource: map[int64]int64{}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.source_id, COUNT(*) FROM items i
		LEFT JOIN item_state st ON st.item_id = i.id
		WHERE COALESCE(st.is_read, 0) = 0 GROUP BY i.source_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		c.BySource[id] = n
		c.Unread += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM item_state WHERE is_starred=1`).Scan(&c.Starred); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM items`).Scan(&c.MaxID); err != nil {
		return nil, err
	}
	return c, nil
}

// ── Full text ─────────────────────────────────────────────────────────────────

// FullTextStatus returns "", "ok" or "failed" for an item.
func (s *Store) FullTextStatus(ctx context.Context, id int64) (string, error) {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM item_fulltext WHERE item_id=?`, id).Scan(&st)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return st, err
}

// SetFullText stores extracted article HTML and re-indexes the item's text.
// A failure is recorded too, so the prefetcher does not retry forever.
func (s *Store) SetFullText(ctx context.Context, id int64, html, text string, readingSecs int64, extractErr error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if extractErr != nil {
		msg := extractErr.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO item_fulltext (item_id, status, error, fetched_at) VALUES (?, 'failed', ?, ?)
			ON CONFLICT(item_id) DO UPDATE SET status='failed', error=excluded.error, fetched_at=excluded.fetched_at
			WHERE item_fulltext.status <> 'ok'`, id, msg, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	var title, author, oldText string
	if err := tx.QueryRowContext(ctx, `SELECT title, author, content_text FROM items WHERE id=?`, id).
		Scan(&title, &author, &oldText); err != nil {
		return err
	}
	if err := ftsDelete(ctx, tx, id, title, author, oldText); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET content_text=?, reading_secs=MAX(reading_secs, ?) WHERE id=?`,
		text, readingSecs, id); err != nil {
		return err
	}
	if err := ftsInsert(ctx, tx, id, title, author, text); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO item_fulltext (item_id, status, content_html, error, fetched_at) VALUES (?, 'ok', ?, '', ?)
		ON CONFLICT(item_id) DO UPDATE SET status='ok', content_html=excluded.content_html, error='', fetched_at=excluded.fetched_at`,
		id, html, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ItemsNeedingFullText lists recent items of full-text sources not yet attempted.
func (s *Store) ItemsNeedingFullText(ctx context.Context, sourceID int64, limit int) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.url FROM items i
		LEFT JOIN item_fulltext f ON f.item_id = i.id
		WHERE i.source_id = ? AND f.item_id IS NULL AND i.kind = 'article'
		ORDER BY i.id DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Item
	for rows.Next() {
		it := &model.Item{SourceID: sourceID}
		if err := rows.Scan(&it.ID, &it.URL); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpsertItems stores a batch. A bad item is logged and skipped rather than
// failing the whole poll. Returns the ids of newly inserted items.
func (s *Store) UpsertItems(ctx context.Context, items []*model.Item) []int64 {
	var newIDs []int64
	for _, it := range items {
		id, isNew, err := s.UpsertItem(ctx, it)
		if err != nil {
			slog.Error("upsert item", "source", it.SourceID, "url", it.URL, "err", err)
			continue
		}
		if isNew {
			newIDs = append(newIDs, id)
		}
	}
	return newIDs
}

// CountItems counts the items a query would match (ignoring paging).
func (s *Store) CountItems(ctx context.Context, q ItemQuery) (int64, error) {
	where, args := q.where()
	sqlq := `SELECT COUNT(*) FROM items i LEFT JOIN item_state st ON st.item_id = i.id`
	if where != "" {
		sqlq += ` WHERE ` + where
	}
	var n int64
	err := s.db.QueryRowContext(ctx, sqlq, args...).Scan(&n)
	return n, err
}
