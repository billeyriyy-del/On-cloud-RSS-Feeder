package storage

// Connection pragmas travel in the DSN (see dsn) so every pooled connection gets
// them, not just the first one. journal_mode is persistent and set once in New.

// migrations are applied in order, each inside its own transaction, and recorded
// in schema_migrations. Never edit a shipped migration — append a new one.
var migrations = []string{
	// ── 1: original schema (idempotent so pre-migration databases adopt it) ──
	`
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
-- Manual insert/delete required (cascade does not reach FTS5).
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
`,
	// ── 2: exact sync log, richer items, per-field LWW state, push + full text ──
	`
-- Items: presentation fields for timelines, podcasts and video.
ALTER TABLE items ADD COLUMN summary          TEXT    NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN image_url        TEXT    NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN kind             TEXT    NOT NULL DEFAULT 'article';
ALTER TABLE items ADD COLUMN lang             TEXT    NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN reading_secs     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN entry_updated_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN updated_at       INTEGER NOT NULL DEFAULT 0;
UPDATE items SET updated_at = fetched_at;
CREATE INDEX idx_items_timeline ON items(published_at DESC, id DESC);

CREATE TABLE item_enclosures (
    item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    url      TEXT    NOT NULL,
    type     TEXT    NOT NULL DEFAULT '',
    length   INTEGER NOT NULL DEFAULT 0,
    duration INTEGER NOT NULL DEFAULT 0,
    UNIQUE (item_id, url)            -- rowid keeps feed order
);

-- Extracted full text lives apart from feed content, on its own lifecycle.
CREATE TABLE item_fulltext (
    item_id      INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    status       TEXT    NOT NULL CHECK(status IN ('ok','failed')),
    content_html TEXT    NOT NULL DEFAULT '',
    error        TEXT    NOT NULL DEFAULT '',
    fetched_at   INTEGER NOT NULL
);

-- Sources: display metadata, full-text opt-in, error surface, WebSub lease.
ALTER TABLE sources ADD COLUMN site_url          TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN icon_url          TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN description       TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN kind              TEXT    NOT NULL DEFAULT 'article';
ALTER TABLE sources ADD COLUMN fetch_full_text   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sources ADD COLUMN last_error        TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN websub_hub        TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN websub_topic      TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN websub_secret     TEXT    NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN websub_expires_at INTEGER NOT NULL DEFAULT 0;

-- State: when each flag last changed (client clock) for per-field last-write-wins.
ALTER TABLE item_state ADD COLUMN read_changed_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE item_state ADD COLUMN star_changed_at INTEGER NOT NULL DEFAULT 0;
UPDATE item_state SET read_changed_at = COALESCE(read_at, updated_at),
                      star_changed_at = COALESCE(starred_at, updated_at);

-- Change log: every client-visible write appends a row (via triggers below).
-- seq is the sync cursor. It is strictly increasing and, because SQLite has one
-- writer, commit order equals seq order — so "seq > cursor" never skips a row,
-- no matter how many changes share a second. Deletes here replace tombstones.
CREATE TABLE change_log (
    seq       INTEGER PRIMARY KEY AUTOINCREMENT,
    entity    TEXT    NOT NULL CHECK(entity IN ('item','state','folder','source')),
    entity_id INTEGER NOT NULL,
    op        TEXT    NOT NULL CHECK(op IN ('upsert','delete')),
    at        INTEGER NOT NULL
);
CREATE INDEX idx_change_log_entity ON change_log(entity, entity_id);
CREATE INDEX idx_change_log_at     ON change_log(at);

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
) WITHOUT ROWID;
-- Highest seq of a delete that compaction has dropped; cursors below it must reset.
INSERT INTO meta (key, value) VALUES ('sync_floor', 0);

-- Backfill so existing data syncs to new clients.
INSERT INTO change_log (entity, entity_id, op, at)
    SELECT entity_type, entity_id, 'delete', deleted_at FROM tombstones
    WHERE entity_type IN ('item','folder','source') ORDER BY deleted_at, id;
INSERT INTO change_log (entity, entity_id, op, at)
    SELECT 'folder', id, 'upsert', updated_at FROM folders ORDER BY id;
INSERT INTO change_log (entity, entity_id, op, at)
    SELECT 'source', id, 'upsert', updated_at FROM sources ORDER BY id;
INSERT INTO change_log (entity, entity_id, op, at)
    SELECT 'item', id, 'upsert', fetched_at FROM items ORDER BY id;
INSERT INTO change_log (entity, entity_id, op, at)
    SELECT 'state', item_id, 'upsert', updated_at FROM item_state ORDER BY updated_at, item_id;
DROP TABLE tombstones;

-- Triggers. FK actions (ON DELETE CASCADE / SET NULL) fire these too, so deleting a
-- source logs its items, and deleting a folder logs the sources it releases.
CREATE TRIGGER trg_items_ins AFTER INSERT ON items BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('item', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_items_upd AFTER UPDATE OF url, title, author, summary, image_url, kind, lang,
        reading_secs, published_at, content_html, updated_at ON items BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('item', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_items_del AFTER DELETE ON items BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('item', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_fulltext_ok AFTER INSERT ON item_fulltext WHEN NEW.status = 'ok' BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('item', NEW.item_id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_fulltext_upd AFTER UPDATE ON item_fulltext WHEN NEW.status = 'ok' BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('item', NEW.item_id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_state_ins AFTER INSERT ON item_state BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('state', NEW.item_id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_state_upd AFTER UPDATE ON item_state BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('state', NEW.item_id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_folders_ins AFTER INSERT ON folders BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('folder', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_folders_upd AFTER UPDATE ON folders BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('folder', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_folders_del AFTER DELETE ON folders BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('folder', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_sources_ins AFTER INSERT ON sources BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('source', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
-- Only user-visible columns: routine poll bookkeeping must not flood the log.
CREATE TRIGGER trg_sources_upd AFTER UPDATE ON sources
    WHEN OLD.type IS NOT NEW.type OR OLD.url IS NOT NEW.url OR OLD.title IS NOT NEW.title
      OR OLD.folder_id IS NOT NEW.folder_id OR OLD.scraper_config IS NOT NEW.scraper_config
      OR OLD.is_dead IS NOT NEW.is_dead OR OLD.site_url IS NOT NEW.site_url
      OR OLD.icon_url IS NOT NEW.icon_url OR OLD.description IS NOT NEW.description
      OR OLD.kind IS NOT NEW.kind OR OLD.fetch_full_text IS NOT NEW.fetch_full_text
      OR (OLD.websub_expires_at > 0) IS NOT (NEW.websub_expires_at > 0)
      OR (OLD.last_error = '') IS NOT (NEW.last_error = '')
BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('source', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER));
END;
CREATE TRIGGER trg_sources_del AFTER DELETE ON sources BEGIN
    INSERT INTO change_log (entity, entity_id, op, at) VALUES ('source', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER));
END;
`,
	// ── 3: remember swept entries so feeds that keep long archives (podcasts,
	// full-archive blogs) do not resurrect them as unread after retention ──
	`
CREATE TABLE swept_guids (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    guid      TEXT    NOT NULL,
    PRIMARY KEY (source_id, guid)
) WITHOUT ROWID;
`,
}
