package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbedMigration verifies that opening a pre-v2 database wipes the old
// vector store, clears embedding tracking, recreates the vec table with chunk
// columns, and stamps the new schema version.
func TestEmbedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")

	// Build a v1-style database: old single-vector layout, a tracked message,
	// and user_version = 1.
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	_, err = raw.Exec(`
		CREATE VIRTUAL TABLE messages_vec USING vec0(
			embedding float[384],
			+message_rowid INTEGER
		);
		CREATE TABLE embedded_messages (message_rowid INTEGER PRIMARY KEY);
		INSERT INTO embedded_messages(message_rowid) VALUES (1);
		PRAGMA user_version = 1;
	`)
	if err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}
	raw.Close()

	// openDB runs initSchema, which should migrate.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if ver != embedSchemaVersion {
		t.Errorf("user_version = %d, want %d", ver, embedSchemaVersion)
	}

	var tracked int
	if err := db.QueryRow("SELECT COUNT(*) FROM embedded_messages").Scan(&tracked); err != nil {
		t.Fatalf("count embedded_messages: %v", err)
	}
	if tracked != 0 {
		t.Errorf("embedded_messages not cleared: %d rows remain", tracked)
	}

	// The recreated vec table must accept the chunked layout (5 columns).
	vec, _ := serializeVec(make([]float32, embeddingDim))
	_, err = db.Exec(
		"INSERT INTO messages_vec(embedding, message_rowid, chunk_index, chunk_start, chunk_end) VALUES (?, 1, 0, 0, 10)",
		vec,
	)
	if err != nil {
		t.Errorf("vec table missing chunk columns: %v", err)
	}
}

// TestFreshDBHasVecSchema verifies a brand-new database comes up at the current
// version with the chunked vec table.
func TestFreshDBHasVecSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.sqlite")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if ver != embedSchemaVersion {
		t.Errorf("fresh DB user_version = %d, want %d", ver, embedSchemaVersion)
	}

	vec, _ := serializeVec(make([]float32, embeddingDim))
	if _, err := db.Exec(
		"INSERT INTO messages_vec(embedding, message_rowid, chunk_index, chunk_start, chunk_end) VALUES (?, 1, 0, 0, 10)",
		vec,
	); err != nil {
		t.Errorf("fresh vec table missing chunk columns: %v", err)
	}
}

// TestSessionDeleteCascadeIsIndexed guards the FK cascade from messages to
// tool_uses. Without an index on tool_uses(message_id), every message deleted
// during a session re-index runs a full scan of tool_uses, which turns a
// re-index into hours on a large database.
func TestSessionDeleteCascadeIsIndexed(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("EXPLAIN QUERY PLAN DELETE FROM messages WHERE session_id = 'x'")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(detail, "SCAN tool_uses") {
			t.Fatalf(
				"message delete cascade scans tool_uses: %q (missing index on tool_uses.message_id)",
				detail,
			)
		}
	}
}
