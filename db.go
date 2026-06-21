package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

// embedSchemaVersion is bumped whenever the embedding model, pooling, or vec
// table layout changes, forcing a full re-embed on next index.
//
//	1: all-MiniLM-L6-v2, mean pooling, one vector per message
//	2: snowflake-arctic-embed-s, CLS pooling, chunked (multiple vectors/message)
const embedSchemaVersion = 2

// RunContext is passed to every subcommand via kong.
type RunContext struct {
	DB   *sql.DB
	JSON bool
}

func init() {
	sqlite_vec.Auto()
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open(
		"sqlite3",
		path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL&_cache_size=-64000",
	)
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Add the provenance column to older DBs, and stamp already-imported
	// claude.ai sessions (which were marked via project_name before this column).
	if added, err := ensureColumn(
		db, "sessions", "provenance", "TEXT NOT NULL DEFAULT 'claude_code'",
	); err != nil {
		return err
	} else if added {
		if _, err := db.Exec(
			"UPDATE sessions SET provenance = 'claude_ai' WHERE project_name = 'claude.ai'",
		); err != nil {
			return err
		}
	}

	// Migrate the vector store when the embedding model/layout changes. All old
	// vectors are invalid across models, so drop and recreate the vec table and
	// clear the embedding-tracking table to force a full re-embed on next index.
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return err
	}
	if ver < embedSchemaVersion {
		if _, err := db.Exec("DROP TABLE IF EXISTS messages_vec"); err != nil {
			return err
		}
		if _, err := db.Exec("DELETE FROM embedded_messages"); err != nil {
			return err
		}
	}
	if _, err := db.Exec(vecSchema); err != nil {
		return err
	}
	if ver < embedSchemaVersion {
		if _, err := db.Exec(
			fmt.Sprintf("PRAGMA user_version = %d", embedSchemaVersion),
		); err != nil {
			return err
		}
		// Nothing to backfill after a model migration — both tables are empty.
		return nil
	}

	// One-time backfill: populate embedded_messages from messages_vec rows that
	// still correspond to existing messages. Stale entries in messages_vec (from
	// re-indexed sessions whose old embeddings were never cleaned up) are pruned.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM embedded_messages").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		// Prune stale embeddings whose messages no longer exist.
		db.Exec(`DELETE FROM messages_vec WHERE message_rowid NOT IN (SELECT rowid FROM messages)`)
		_, err := db.Exec(`
			INSERT OR IGNORE INTO embedded_messages(message_rowid)
			SELECT mv.message_rowid FROM messages_vec mv
			WHERE mv.message_rowid IN (SELECT rowid FROM messages)`)
		return err
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    project_path TEXT NOT NULL,
    project_name TEXT NOT NULL,
    provenance TEXT NOT NULL DEFAULT 'claude_code',
    model TEXT,
    git_branch TEXT,
    started_at TEXT,
    updated_at TEXT,
    source_path TEXT NOT NULL,
    source_mtime REAL NOT NULL,
    source_size INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_id TEXT,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    is_compact_summary INTEGER DEFAULT 0,
    input_tokens INTEGER,
    output_tokens INTEGER
);

CREATE TABLE IF NOT EXISTS tool_uses (
    id TEXT PRIMARY KEY,
    message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    tool_input_summary TEXT
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='rowid',
    tokenize='porter unicode61'
);

-- Triggers to keep FTS in sync with messages table.
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_role ON messages(role);
CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
CREATE INDEX IF NOT EXISTS idx_tool_uses_session ON tool_uses(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_uses_name ON tool_uses(tool_name);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_name);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at);

-- Track which messages have embeddings (avoids slow LEFT JOIN on vec0 virtual table).
CREATE TABLE IF NOT EXISTS embedded_messages (
    message_rowid INTEGER PRIMARY KEY
);

-- Track all scanned files so we skip empty/unparseable ones on re-index.
CREATE TABLE IF NOT EXISTS indexed_files (
    path TEXT PRIMARY KEY,
    mtime REAL NOT NULL,
    size INTEGER NOT NULL
);
`

// vecSchema is created/recreated separately so a model migration can drop and
// rebuild it. One row per message chunk; chunk_start/chunk_end are character
// offsets into messages.content for snippet rendering.
const vecSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_vec USING vec0(
    embedding float[384],
    +message_rowid INTEGER,
    +chunk_index INTEGER,
    +chunk_start INTEGER,
    +chunk_end INTEGER
);
`

// expandPath replaces a leading ~ with the user's home directory.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

// dirOf returns the directory component of a path.
func dirOf(p string) string {
	return filepath.Dir(p)
}

// ensureColumn adds a column to a table if it's not already present, returning
// whether it was added. Used for lightweight schema migrations on existing DBs.
func ensureColumn(db *sql.DB, table, col, def string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	rows.Close()
	if _, err := db.Exec(
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def),
	); err != nil {
		return false, err
	}
	return true, nil
}
