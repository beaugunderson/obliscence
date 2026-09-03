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
		if _, err := db.Exec("DROP TABLE IF EXISTS messages_vec_owner"); err != nil {
			return err
		}
		if _, err := db.Exec("DELETE FROM embedded_messages"); err != nil {
			return err
		}
	}
	if _, err := db.Exec(vecSchema); err != nil {
		return err
	}
	if _, err := db.Exec(vecOwnerSchema); err != nil {
		return err
	}
	if err := backfillVecOwner(db); err != nil {
		return err
	}

	// head_sha records a hash of the first bytes of each indexed transcript, so a
	// file that has only grown can be told apart from one that was rewritten.
	// Without it the indexer has to assume any change might be a rewrite and
	// tear the whole session down before rebuilding it.
	if _, err := ensureColumn(db, "indexed_files", "head_sha", "TEXT"); err != nil {
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
-- tool_uses.message_id is an ON DELETE CASCADE foreign key. Without an index
-- on it, deleting one session makes SQLite scan the whole tool_uses table once
-- per deleted message to find its children, which turns a session rebuild into
-- hours on a long transcript.
CREATE INDEX IF NOT EXISTS idx_tool_uses_message ON tool_uses(message_id);
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

// vecOwnerSchema maps each vector row back to the message it came from, in an
// ordinary B-tree. message_rowid inside messages_vec is an AUXILIARY column,
// which sqlite-vec cannot filter on: any "WHERE message_rowid = ?" degrades to
// a full scan that opens a blob per vector row to read the value back. Deleting
// a session's vectors that way costs minutes on a large index. This table gives
// the delete path an indexed lookup and lets it delete by vec rowid, which vec0
// does handle as a point operation.
const vecOwnerSchema = `
CREATE TABLE IF NOT EXISTS messages_vec_owner (
    vec_rowid INTEGER PRIMARY KEY,
    message_rowid INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_vec_owner_message ON messages_vec_owner(message_rowid);
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

// backfillVecOwner populates messages_vec_owner for databases whose vectors were
// written before the owner map existed. Without it, those vectors are invisible
// to the delete path and would survive a re-index as duplicates.
//
// The mapping is read out of sqlite-vec's own shadow tables, which are ordinary
// B-trees: messages_vec_rowids.rowid is the vec rowid and
// messages_vec_auxiliary.value00 is the first auxiliary column, message_rowid.
// Going through the virtual table instead would mean the same per-row blob reads
// this change exists to avoid. If the shadow layout is not what we expect, fall
// back to the slow but version-independent read.
func backfillVecOwner(db *sql.DB) error {
	var owned int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages_vec_owner").Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return nil
	}
	var vectors int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages_vec_rowids").Scan(&vectors); err != nil {
		// No shadow table to read: nothing embedded yet, or a layout we do not know.
		return nil
	}
	if vectors == 0 {
		return nil
	}
	if _, err := db.Exec(`
		INSERT OR REPLACE INTO messages_vec_owner(vec_rowid, message_rowid)
		SELECT rowid, value00 FROM messages_vec_auxiliary WHERE value00 IS NOT NULL`); err == nil {
		return nil
	}
	_, err := db.Exec(`
		INSERT OR REPLACE INTO messages_vec_owner(vec_rowid, message_rowid)
		SELECT rowid, message_rowid FROM messages_vec`)
	return err
}
