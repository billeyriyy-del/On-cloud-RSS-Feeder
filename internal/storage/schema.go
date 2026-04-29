package storage

// pragmas are applied to every new connection.
const pragmas = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA cache_size   = -20000;
PRAGMA temp_store   = MEMORY;
PRAGMA mmap_size    = 268435456;
`

// initSQL creates all tables and indexes idempotently.
const initSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS folders (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    parent_id  INTEGER REFERENCES folders(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sources (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    type           TEXT    NOT NULL CHECK(type IN ('rss','html')),
    url            TEXT    NOT NULL UNIQUE,
    title          TEXT    NOT NULL DEFAULT '',
    folder_id      INTEGER REFERENCES folders(id) ON DELETE SET NULL,
    scraper_config TEXT,
    next_poll_at   INTEGER NOT NULL DEFAULT 0,
    last_poll_at   INTEGER,
    etag           TEXT    NOT NULL DEFAULT '',
    last_modified  TEXT    NOT NULL DEFAULT '',
    consec_fails   INTEGER NOT NULL DEFAULT 0,
    is_dead        INTEGER NOT NULL DEFAULT 0,
    poll_interval  INTEGER NOT NULL DEFAULT 3600,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS items (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id    INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    guid         TEXT    NOT NULL,
    url          TEXT    NOT NULL,
    title        TEXT    NOT NULL DEFAULT '',
    author       TEXT    NOT NULL DEFAULT '',
    published_at INTEGER NOT NULL,
    fetched_at   INTEGER NOT NULL,
    content_html TEXT    NOT NULL DEFAULT '',
    content_text TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    UNIQUE(source_id, guid)
);

CREATE INDEX IF NOT EXISTS idx_items_source_published ON items(source_id, published_at DESC);
CREATE INDEX IF NOT EXISTS idx_items_fetched_at       ON items(fetched_at);

-- FTS5 external-content table: index is stored here, content lives in items.
-- Manual insert/delete required (cascade does not work for FTS5).
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
    title,
    author,
    content_text,
    content     = 'items',
    content_rowid = 'id',
    tokenize    = 'porter unicode61'
);

CREATE TABLE IF NOT EXISTS item_state (
    item_id    INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    is_read    INTEGER NOT NULL DEFAULT 0,
    is_starred INTEGER NOT NULL DEFAULT 0,
    read_at    INTEGER,
    starred_at INTEGER,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_item_state_updated ON item_state(updated_at);

CREATE TABLE IF NOT EXISTS tombstones (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT    NOT NULL,
    entity_id   INTEGER NOT NULL,
    deleted_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tombstones_deleted ON tombstones(deleted_at);
`
