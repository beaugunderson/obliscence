package main

import (
	"database/sql"
	"path/filepath"
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
