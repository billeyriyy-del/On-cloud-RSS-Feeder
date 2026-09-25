// Package storage is Noema's SQLite persistence layer.
//
// Invariants worth knowing before editing:
//   - items_fts is an external-content FTS5 table synced by hand. Any code that
//     deletes items or changes items.title/author/content_text must update the FTS
//     index in the same transaction (see ftsDelete / ftsInsert). FK cascades do
//     NOT reach FTS5 — that is why DeleteSource and the retention sweep walk items.
//   - change_log is maintained by triggers (schema.go). Sync correctness depends on
//     every client-visible write going through a table that has one.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"rssfeeder/internal/model"

	_ "modernc.org/sqlite"
)

// Store is the single concrete storage type.
type Store struct {
	db  *sql.DB
	now func() int64 // overridable in tests
}

// dsn builds a driver-agnostic SQLite DSN (both modernc and ncruces accept
// _pragma), so per-connection pragmas survive the pool reopening a connection.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range []string{
		"foreign_keys(1)",
		"busy_timeout(5000)",
		"synchronous(NORMAL)",
		"cache_size(-20000)",
		"temp_store(MEMORY)",
		"mmap_size(268435456)",
	} {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// New opens (or creates) the SQLite database at path and runs migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// One connection: SQLite has a single writer anyway, and at personal scale
	// serialising reads behind it is simpler than managing SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	s := &Store{db: db, now: func() int64 { return time.Now().Unix() }}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is reachable (for /healthz).
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		version := i + 1
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, s.now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d commit: %w", version, err)
		}
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanner unifies *sql.Row and *sql.Rows for the scan helpers.
type scanner interface {
	Scan(dest ...any) error
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders returns "?,?,?" with n marks and the ids as []any.
func placeholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// chunks splits ids so IN lists stay well under SQLite's variable limit.
func chunks(ids []int64, n int) [][]int64 {
	var out [][]int64
	for len(ids) > n {
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
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

// ftsDelete removes an item's current values from the external-content index.
// The values must be exactly what was indexed, so read them from items first.
func ftsDelete(ctx context.Context, x execer, id int64, title, author, text string) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO items_fts(items_fts, rowid, title, author, content_text) VALUES('delete',?,?,?,?)`,
		id, title, author, text)
	return err
}

func ftsInsert(ctx context.Context, x execer, id int64, title, author, text string) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO items_fts(rowid, title, author, content_text) VALUES (?,?,?,?)`,
		id, title, author, text)
	return err
}

// ftsDeleteWhere removes every matching item from the FTS index before a bulk delete.
func ftsDeleteWhere(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, title, author, content_text FROM items WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	type mini struct {
		id                  int64
		title, author, text string
	}
	var todo []mini
	for rows.Next() {
		var m mini
		if err := rows.Scan(&m.id, &m.title, &m.author, &m.text); err != nil {
			rows.Close()
			return nil, err
		}
		todo = append(todo, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(todo))
	for i, m := range todo {
		if err := ftsDelete(ctx, tx, m.id, m.title, m.author, m.text); err != nil {
			return nil, fmt.Errorf("fts delete %d: %w", m.id, err)
		}
		ids[i] = m.id
	}
	return ids, nil
}
