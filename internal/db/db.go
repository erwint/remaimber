package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	session_id      TEXT PRIMARY KEY,
	project_key     TEXT NOT NULL,
	project_path    TEXT,
	custom_title    TEXT,
	first_prompt    TEXT,
	git_branch      TEXT,
	cwd             TEXT,
	started_at      TEXT,
	ended_at        TEXT,
	message_count   INTEGER DEFAULT 0,
	file_mtime      REAL,
	file_size        INTEGER,
	last_byte_offset INTEGER DEFAULT 0,
	imported_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS messages (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id   TEXT NOT NULL,
	uuid         TEXT,
	parent_uuid  TEXT,
	type         TEXT NOT NULL,
	role         TEXT,
	content_text TEXT,
	content_json TEXT NOT NULL,
	content_hash TEXT,
	timestamp    TEXT,
	FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_uuid
	ON messages(uuid) WHERE uuid IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_hash
	ON messages(session_id, content_hash) WHERE content_hash IS NOT NULL AND uuid IS NULL;

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(type);
CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	content_text,
	content='messages',
	content_rowid='id',
	tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid, content_text) VALUES (new.id, new.content_text);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content_text) VALUES('delete', old.id, old.content_text);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content_text) VALUES('delete', old.id, old.content_text);
	INSERT INTO messages_fts(rowid, content_text) VALUES (new.id, new.content_text);
END;

-- Durable cross-worktree identity, keyed by session_id and decoupled from the
-- sessions table so it can be captured (at SessionStart) before the session is
-- imported. No FK on purpose: the identity may exist before the session row.
CREATE TABLE IF NOT EXISTS session_identity (
	session_id    TEXT PRIMARY KEY,
	repo_id       TEXT,
	subpath       TEXT,
	worktree_root TEXT,
	cwd           TEXT,
	captured_at   TEXT,
	pid           INTEGER,
	ended_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_identity_repo    ON session_identity(repo_id);
CREATE INDEX IF NOT EXISTS idx_identity_subpath ON session_identity(repo_id, subpath);

-- Per-conversation summary segments. A session is a sequence of segments split at
-- context-compaction boundaries and at a size cap; all but the last are frozen
-- (closed). Boundaries are keyed by message uuid as well as id so a later phase
-- can validate them against the active conversation path (rewind/restore).
CREATE TABLE IF NOT EXISTS session_segments (
	session_id  TEXT NOT NULL,
	seq         INTEGER NOT NULL,   -- order within the session, 0-based
	start_id    INTEGER NOT NULL,   -- first content message id (inclusive)
	end_id      INTEGER,            -- last content message id (inclusive)
	start_uuid  TEXT,
	end_uuid    TEXT,
	summary     TEXT,
	msg_count   INTEGER DEFAULT 0,  -- content messages folded into this segment
	high_water  INTEGER DEFAULT 0,  -- last content id folded (for the open segment)
	closed      INTEGER DEFAULT 0,  -- 1 = frozen/immutable
	reason      TEXT,               -- why it closed: 'compaction' | 'sizecap'
	updated_at  TEXT,
	PRIMARY KEY (session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_segments_session ON session_segments(session_id);
`

// migrations are idempotent ALTER statements applied after the schema. SQLite
// has no "ADD COLUMN IF NOT EXISTS", so a duplicate-column error is expected and
// ignored on databases that already have the column.
var migrations = []string{
	`ALTER TABLE sessions ADD COLUMN summary TEXT`,
	`ALTER TABLE sessions ADD COLUMN summary_offset INTEGER DEFAULT 0`,
}

// DBPath returns the default database path.
func DBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude", "remaimber")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "remaimber.db"), nil
}

// Open opens the database, creates schema if needed, and configures WAL mode.
// Checks REMAIMBER_DB env var first, then uses default path.
func Open() (*sql.DB, error) {
	path := os.Getenv("REMAIMBER_DB")
	if path == "" {
		var err error
		path, err = DBPath()
		if err != nil {
			return nil, fmt.Errorf("db path: %w", err)
		}
	}
	return OpenAt(path)
}

// OpenPath opens the database at a specific path, or uses default if empty.
func OpenPath(path string) (*sql.DB, error) {
	if path != "" {
		return OpenAt(path)
	}
	return Open()
}

// OpenAt opens a database at the specified path.
func OpenAt(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Configure for concurrent access
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}

	// Apply idempotent column migrations; ignore "duplicate column" errors.
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migration %q: %w", m, err)
		}
	}

	return db, nil
}
