package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"rssfeeder/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustSource(t *testing.T, s *Store, url string, folder *int64) *model.Source {
	t.Helper()
	src := &model.Source{Type: model.SourceTypeRSS, URL: url, Title: url, FolderID: folder}
	if err := s.CreateSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	return src
}

func mustItem(t *testing.T, s *Store, srcID int64, guid string) int64 {
	t.Helper()
	id, isNew, err := s.UpsertItem(context.Background(), &model.Item{
		SourceID: srcID, GUID: guid, URL: "https://example.com/" + guid, Title: "Title " + guid,
		ContentHTML: "<p>body " + guid + "</p>", ContentText: "body " + guid, PublishedAt: 1_700_000_000,
	})
	if err != nil || !isNew {
		t.Fatalf("upsert %s: new=%v err=%v", guid, isNew, err)
	}
	return id
}

// drain pages through sync from cursor and returns everything seen.
func drain(t *testing.T, s *Store, cursor int64, limit int) (items map[int64]int, tombs map[string]int, last int64, pages int) {
	t.Helper()
	items, tombs = map[int64]int{}, map[string]int{}
	for {
		d, err := s.GetSyncDelta(context.Background(), cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reset {
			t.Fatalf("unexpected reset at cursor %d", cursor)
		}
		pages++
		for _, it := range d.Items {
			items[it.ID]++
		}
		for _, tb := range d.Tombstones {
			tombs[fmt.Sprintf("%s:%d", tb.EntityType, tb.EntityID)]++
		}
		if d.Cursor < cursor {
			t.Fatalf("cursor went backwards: %d -> %d", cursor, d.Cursor)
		}
		cursor = d.Cursor
		if !d.HasMore {
			return items, tombs, cursor, pages
		}
		if pages > 1000 {
			t.Fatal("sync did not terminate")
		}
	}
}

func TestSyncPagingIsExactWithEqualTimestamps(t *testing.T) {
	s := newTestStore(t)
	s.now = func() int64 { return 1_700_000_000 } // every write in the same second
	src := mustSource(t, s, "https://a.example/feed", nil)
	const n = 1234
	for i := 0; i < n; i++ {
		mustItem(t, s, src.ID, fmt.Sprint(i))
	}
	items, _, cursor, pages := drain(t, s, 0, 500)
	if len(items) != n {
		t.Fatalf("got %d distinct items, want %d", len(items), n)
	}
	for id, c := range items {
		if c != 1 {
			t.Fatalf("item %d delivered %d times", id, c)
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}

	// Writes after the drain (same second!) are picked up from the cursor.
	extra := mustItem(t, s, src.ID, "late")
	items, _, _, _ = drain(t, s, cursor, 500)
	if len(items) != 1 || items[extra] != 1 {
		t.Fatalf("incremental sync = %v, want only %d", items, extra)
	}
}

func TestSyncCollapsesRepeatedChangesAndCarriesState(t *testing.T) {
	s := newTestStore(t)
	src := mustSource(t, s, "https://a.example/feed", nil)
	id := mustItem(t, s, src.ID, "x")
	d0, _ := s.GetSyncDelta(context.Background(), 0, 500)
	yes, no := true, false
	for i := 0; i < 5; i++ {
		if _, err := s.ApplyStateOps(context.Background(), []model.StateOp{{ItemID: id, IsRead: &yes, At: int64(100 + 2*i)}, {ItemID: id, IsRead: &no, At: int64(101 + 2*i)}}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.GetSyncDelta(context.Background(), d0.Cursor, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.States) != 1 || d.States[0].IsRead {
		t.Fatalf("states = %+v, want one unread state", d.States)
	}
}

func TestDeletesPropagateAndCompactionForcesReset(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	src := mustSource(t, s, "https://a.example/feed", nil)
	a, b := mustItem(t, s, src.ID, "a"), mustItem(t, s, src.ID, "b")
	_, _, cursor, _ := drain(t, s, 0, 500)

	if err := s.DeleteSource(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	_, tombs, after, _ := drain(t, s, cursor, 500)
	for _, k := range []string{fmt.Sprintf("item:%d", a), fmt.Sprintf("item:%d", b), fmt.Sprintf("source:%d", src.ID)} {
		if tombs[k] != 1 {
			t.Fatalf("missing tombstone %s in %v", k, tombs)
		}
	}
	// FTS must not return deleted items.
	res, err := s.SearchItems(ctx, "body", 10)
	if err != nil || len(res) != 0 {
		t.Fatalf("search after delete = %d results, err %v", len(res), err)
	}

	// Compact away the deletes: an old cursor must reset, a current one must not.
	if _, err := s.CompactChangeLog(ctx, s.now()+10); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetSyncDelta(ctx, cursor, 500)
	if err != nil || !d.Reset {
		t.Fatalf("old cursor: reset=%v err=%v, want reset", d.Reset, err)
	}
	d, err = s.GetSyncDelta(ctx, after, 500)
	if err != nil || d.Reset {
		t.Fatalf("current cursor: reset=%v err=%v, want no reset", d.Reset, err)
	}
	// A fresh client (cursor 0) never needs a reset.
	if d, _ := s.GetSyncDelta(ctx, 0, 500); d.Reset {
		t.Fatal("cursor 0 must not reset")
	}
}

func TestLegacyTimestampCursorNeverSkips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	src := mustSource(t, s, "https://a.example/feed", nil)
	s.now = func() int64 { return 1_700_000_100 }
	mustItem(t, s, src.ID, "old")
	s.now = func() int64 { return 1_700_000_200 }
	newID := mustItem(t, s, src.ID, "new")

	// Trigger timestamps use SQLite's clock, so ask for everything since the
	// log's own time of the newest row.
	var at int64
	s.db.QueryRow(`SELECT at FROM change_log WHERE entity='item' AND entity_id=?`, newID).Scan(&at)
	c, err := s.CursorForTimestamp(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	items, _, _, _ := drain(t, s, c, 500)
	if items[newID] != 1 {
		t.Fatalf("legacy cursor skipped item %d: %v", newID, items)
	}
}

func TestStateLastWriteWinsPerField(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	src := mustSource(t, s, "https://a.example/feed", nil)
	id := mustItem(t, s, src.ID, "x")
	s.now = func() int64 { return 10_000 }
	yes, no := true, false

	apply := func(op model.StateOp) *model.ItemState {
		t.Helper()
		out, err := s.ApplyStateOps(ctx, []model.StateOp{op})
		if err != nil || len(out) != 1 {
			t.Fatalf("apply: %v %v", out, err)
		}
		return out[0]
	}
	st := apply(model.StateOp{ItemID: id, IsRead: &yes, At: 9_000})
	if !st.IsRead || *st.ReadAt != 9_000 {
		t.Fatalf("read: %+v", st)
	}
	// An older offline "unread" (from another device) loses.
	if st = apply(model.StateOp{ItemID: id, IsRead: &no, At: 8_000}); !st.IsRead {
		t.Fatal("older write won")
	}
	// Star is an independent field: an old star still applies.
	if st = apply(model.StateOp{ItemID: id, IsStarred: &yes, At: 8_500}); !st.IsStarred || !st.IsRead {
		t.Fatalf("star: %+v", st)
	}
	// A far-future clock is clamped to server time, so it can't lock the field.
	if st = apply(model.StateOp{ItemID: id, IsRead: &no, At: 99_999_999}); st.IsRead || st.ReadChangedAt != 10_000 {
		t.Fatalf("clamp: %+v", st)
	}
	if st = apply(model.StateOp{ItemID: id, IsRead: &yes, At: 10_001}); !st.IsRead {
		t.Fatal("newer write lost")
	}
	// Unknown items are skipped, not errors.
	if out, err := s.ApplyStateOps(ctx, []model.StateOp{{ItemID: 424242, IsRead: &yes}}); err != nil || len(out) != 0 {
		t.Fatalf("unknown item: %v %v", out, err)
	}
}

func TestMarkReadScopesFoldersRecursivelyAndUndoes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	parent := &model.Folder{Name: "News"}
	s.CreateFolder(ctx, parent)
	child := &model.Folder{Name: "Tech", ParentID: &parent.ID}
	s.CreateFolder(ctx, child)
	inChild := mustSource(t, s, "https://child.example/feed", &child.ID)
	outside := mustSource(t, s, "https://other.example/feed", nil)
	a := mustItem(t, s, inChild.ID, "a")
	mustItem(t, s, inChild.ID, "b")
	o := mustItem(t, s, outside.ID, "o")

	ids, err := s.MarkRead(ctx, MarkScope{FolderID: parent.ID, Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("marked %v, want the 2 items under the nested folder", ids)
	}
	if st, _ := s.GetItemState(ctx, o); st.IsRead {
		t.Fatal("item outside folder marked read")
	}
	// Idempotent: nothing left to mark.
	if again, _ := s.MarkRead(ctx, MarkScope{FolderID: parent.ID, Read: true}); len(again) != 0 {
		t.Fatalf("second mark changed %v", again)
	}
	// Undo restores exactly those ids.
	undone, err := s.MarkRead(ctx, MarkScope{IDs: ids, Read: false, At: s.now() + 1})
	if err != nil || len(undone) != 2 {
		t.Fatalf("undo: %v %v", undone, err)
	}
	if st, _ := s.GetItemState(ctx, a); st.IsRead {
		t.Fatal("undo did not restore unread")
	}
	c, _ := s.GetCounts(ctx)
	if c.Unread != 3 {
		t.Fatalf("unread = %d, want 3", c.Unread)
	}
}

func TestUpsertItemAppliesPublisherEditsOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	src := mustSource(t, s, "https://a.example/feed", nil)
	base := model.Item{SourceID: src.ID, GUID: "g", URL: "https://a.example/1", Title: "Teh title",
		ContentHTML: "<p>zebra</p>", ContentText: "zebra", PublishedAt: 1, EntryUpdAt: 100}
	it := base
	id, _, _ := s.UpsertItem(ctx, &it)

	same := base
	same.Title = "changed but not updated"
	if _, isNew, _ := s.UpsertItem(ctx, &same); isNew {
		t.Fatal("duplicate treated as new")
	}
	if v, _ := s.GetItem(ctx, id, false); v.Title != "Teh title" {
		t.Fatalf("unchanged <updated> must not rewrite, got %q", v.Title)
	}

	edited := base
	edited.Title, edited.ContentText, edited.ContentHTML, edited.EntryUpdAt = "The title", "giraffe", "<p>giraffe</p>", 200
	s.UpsertItem(ctx, &edited)
	if v, _ := s.GetItem(ctx, id, false); v.Title != "The title" {
		t.Fatalf("edit not applied: %q", v.Title)
	}
	if r, _ := s.SearchItems(ctx, "zebra", 10); len(r) != 0 {
		t.Fatal("stale FTS entry after edit")
	}
	if r, _ := s.SearchItems(ctx, "giraf", 10); len(r) != 1 {
		t.Fatal("prefix search on edited text failed")
	}
}

func TestFTSQueryIsSafe(t *testing.T) {
	s := newTestStore(t)
	src := mustSource(t, s, "https://a.example/feed", nil)
	mustItem(t, s, src.ID, "q")
	for _, q := range []string{`c++`, `"unbalanced`, `AND OR NOT`, `a:b`, `*`, `(`, `don't`, `NEAR(`} {
		if _, err := s.SearchItems(context.Background(), q, 5); err != nil {
			t.Errorf("query %q: %v", q, err)
		}
	}
}

func TestFolderDeleteReleasesSourcesInSync(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	f := &model.Folder{Name: "F"}
	s.CreateFolder(ctx, f)
	src := mustSource(t, s, "https://a.example/feed", &f.ID)
	d0, _ := s.GetSyncDelta(ctx, 0, 500)
	if err := s.DeleteFolder(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetSyncDelta(ctx, d0.Cursor, 500)
	if len(d.Sources) != 1 || d.Sources[0].ID != src.ID || d.Sources[0].FolderID != nil {
		t.Fatalf("sources in delta = %+v", d.Sources)
	}
	if len(d.Tombstones) != 1 || d.Tombstones[0].EntityType != "folder" {
		t.Fatalf("tombstones = %+v", d.Tombstones)
	}
}

func TestRetentionKeepsStarred(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	src := mustSource(t, s, "https://a.example/feed", nil)
	keep, drop := mustItem(t, s, src.ID, "keep"), mustItem(t, s, src.ID, "drop")
	yes := true
	s.ApplyStateOps(ctx, []model.StateOp{{ItemID: keep, IsStarred: &yes}})
	n, err := s.SweepExpiredItems(ctx, s.now()+1)
	if err != nil || n != 1 {
		t.Fatalf("swept %d, err %v", n, err)
	}
	if v, _ := s.GetItem(ctx, drop, false); v != nil {
		t.Fatal("expired item survived")
	}
	if v, _ := s.GetItem(ctx, keep, false); v == nil {
		t.Fatal("starred item swept")
	}
}

// TestMigratesPreMigrationDatabase opens a database created by the original
// schema (no migration rows) and checks existing data flows into sync.
func TestMigratesPreMigrationDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);` +
		migrations[0] + `CREATE INDEX idx_tombstones_deleted ON tombstones(deleted_at);
		INSERT INTO sources (type,url,title,created_at,updated_at) VALUES ('rss','https://old.example/feed','Old',1,1);
		INSERT INTO items (source_id,guid,url,title,published_at,fetched_at,content_text,created_at) VALUES (1,'g','https://old.example/1','Hello',5,5,'legacy words',5);
		INSERT INTO items_fts(rowid,title,author,content_text) VALUES (1,'Hello','','legacy words');
		INSERT INTO item_state (item_id,is_read,is_starred,read_at,updated_at) VALUES (1,1,0,7,7);
		INSERT INTO tombstones (entity_type,entity_id,deleted_at) VALUES ('item',99,3);`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.SchemaVersion(context.Background()); v != len(migrations) {
		t.Fatalf("schema version %d", v)
	}
	d, err := s.GetSyncDelta(context.Background(), 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sources) != 1 || len(d.Items) != 1 || len(d.States) != 1 || len(d.Tombstones) != 1 {
		t.Fatalf("backfill: %d sources %d items %d states %d tombstones",
			len(d.Sources), len(d.Items), len(d.States), len(d.Tombstones))
	}
	if d.States[0].ReadChangedAt != 7 {
		t.Fatalf("read_changed_at backfill = %d", d.States[0].ReadChangedAt)
	}
	if r, _ := s.SearchItems(context.Background(), "legacy", 5); len(r) != 1 {
		t.Fatal("FTS lost across migration")
	}
	// Re-opening is a no-op.
	s.Close()
	if s, err = New(path); err != nil {
		t.Fatal(err)
	}
}
