package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSession writes a Claude Code style JSONL transcript with n user
// messages (ids u1..un) for session sid and returns its path.
func writeSession(t *testing.T, dir, sid string, n int) string {
	t.Helper()
	var lines []string
	for i := 1; i <= n; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"user","uuid":"u%d","parentUuid":"","sessionId":"%s","cwd":"/tmp/proj","timestamp":"2026-01-01T00:00:%02dZ","message":{"role":"user","content":"user message number %d with enough text"}}`,
			i,
			sid,
			i,
			i,
		))
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// embedFake records a placeholder embedding for message id so the test can
// tell whether a re-index preserved or discarded it.
func embedFake(t *testing.T, db *sql.DB, id string) int64 {
	t.Helper()
	var rowid int64
	if err := db.QueryRow("SELECT rowid FROM messages WHERE id = ?", id).Scan(&rowid); err != nil {
		t.Fatalf("rowid of %s: %v", id, err)
	}
	vec, _ := serializeVec(make([]float32, embeddingDim))
	res, err := db.Exec(
		"INSERT INTO messages_vec(embedding, message_rowid, chunk_index, chunk_start, chunk_end) VALUES (?, ?, 0, 0, 10)",
		vec,
		rowid,
	)
	if err != nil {
		t.Fatal(err)
	}
	vecRowid, _ := res.LastInsertId()
	if _, err := db.Exec(
		"INSERT INTO messages_vec_owner(vec_rowid, message_rowid) VALUES (?, ?)", vecRowid, rowid,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO embedded_messages(message_rowid) VALUES (?)",
		rowid,
	); err != nil {
		t.Fatal(err)
	}
	return rowid
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestReindexKeepsExistingMessages: re-indexing a session whose transcript
// grew must keep the existing message rows (same rowids) and their
// embeddings, and add only the new messages.
func TestReindexKeepsExistingMessages(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := writeSession(t, dir, "sess-a", 2)
	if err := indexSingleFile(db, path); err != nil {
		t.Fatal(err)
	}
	rowid1 := embedFake(t, db, "u1")

	// The transcript grows by one message and is re-indexed.
	writeSession(t, dir, "sess-a", 3)
	if err := indexSingleFile(db, path); err != nil {
		t.Fatal(err)
	}

	if got := count(t, db, "SELECT COUNT(*) FROM messages WHERE session_id = 'sess-a'"); got != 3 {
		t.Errorf("messages = %d, want 3", got)
	}
	var again int64
	if err := db.QueryRow("SELECT rowid FROM messages WHERE id = 'u1'").Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != rowid1 {
		t.Errorf("u1 rowid changed %d -> %d; existing rows must be kept", rowid1, again)
	}
	if got := count(
		t,
		db,
		"SELECT COUNT(*) FROM embedded_messages WHERE message_rowid = ?",
		rowid1,
	); got != 1 {
		t.Errorf("embedding tracking for u1 lost")
	}
	if got := count(t, db, "SELECT COUNT(*) FROM messages_vec"); got != 1 {
		t.Errorf("messages_vec rows = %d, want 1 (existing vector kept, nothing dropped)", got)
	}
	var updated string
	db.QueryRow("SELECT updated_at FROM sessions WHERE id = 'sess-a'").Scan(&updated)
	if updated != "2026-01-01T00:00:03Z" {
		t.Errorf("session updated_at = %q, want the newest message timestamp", updated)
	}
}

// TestReindexRemovesStaleMessages: a message that disappeared from the
// transcript is removed along with its embedding rows.
func TestReindexRemovesStaleMessages(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := writeSession(t, dir, "sess-b", 3)
	if err := indexSingleFile(db, path); err != nil {
		t.Fatal(err)
	}
	embedFake(t, db, "u2")
	rowid3 := embedFake(t, db, "u3")

	// The transcript is rewritten with the last message gone.
	writeSession(t, dir, "sess-b", 2)
	if err := indexSingleFile(db, path); err != nil {
		t.Fatal(err)
	}

	if got := count(t, db, "SELECT COUNT(*) FROM messages WHERE id = 'u3'"); got != 0 {
		t.Errorf("stale message u3 still present")
	}
	if got := count(
		t,
		db,
		"SELECT COUNT(*) FROM embedded_messages WHERE message_rowid = ?",
		rowid3,
	); got != 0 {
		t.Errorf("stale embedding tracking for u3 still present")
	}
	if got := count(
		t,
		db,
		"SELECT COUNT(*) FROM messages_vec WHERE message_rowid = ?",
		rowid3,
	); got != 0 {
		t.Errorf("stale vector for u3 still present")
	}
	if got := count(t, db, "SELECT COUNT(*) FROM messages_vec"); got != 1 {
		t.Errorf("messages_vec rows = %d, want 1 (u2 kept)", got)
	}
	if got := count(
		t,
		db,
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'number AND 3'",
	); got != 0 {
		t.Errorf("stale message u3 still in FTS")
	}
}

// TestSessionFilesSkipsAgentTranscripts: top-level agent-*.jsonl files are
// subagent transcripts stamped with their parent's session id, so indexing
// one would clobber the parent session. Discovery must skip them.
func TestSessionFilesSkipsAgentTranscripts(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "11111111-1111-1111-1111-111111111111", 1)
	agent := filepath.Join(dir, "agent-abc123.jsonl")
	if err := os.WriteFile(
		agent,
		[]byte(
			`{"type":"user","uuid":"a1","sessionId":"11111111-1111-1111-1111-111111111111","message":{"role":"user","content":"agent"}}`+"\n",
		),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	got := sessionFiles(dir)
	if len(got) != 1 || filepath.Base(got[0]) != "11111111-1111-1111-1111-111111111111.jsonl" {
		t.Errorf("sessionFiles = %v, want only the session transcript", got)
	}
}

// TestSessionIDFromFilename: a forked session's transcript begins with lines
// copied from its parent, carrying the parent's sessionId, so the file name
// (which Claude Code sets to the session id) is the authoritative id.
func TestSessionIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	parent := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	fork := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	// The fork's file is named after the fork but its lines say parent.
	path := writeSession(t, dir, parent, 2)
	forkPath := filepath.Join(dir, fork+".jsonl")
	if err := os.Rename(path, forkPath); err != nil {
		t.Fatal(err)
	}
	if err := indexSingleFile(db, forkPath); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM sessions WHERE id = ?", fork); got != 1 {
		t.Errorf("session keyed as %s not found; sessions: %d under parent id", fork,
			count(t, db, "SELECT COUNT(*) FROM sessions WHERE id = ?", parent))
	}
}

// TestPruneOrphanVectors: vectors whose message is gone (left behind by an
// earlier delete path) are removed along with their owner and tracking rows.
func TestPruneOrphanVectors(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := writeSession(t, dir, "sess-c", 2)
	if err := indexSingleFile(db, path); err != nil {
		t.Fatal(err)
	}
	embedFake(t, db, "u1")
	orphan := embedFake(t, db, "u2")
	// Delete the message behind u2 directly, leaving its vector orphaned.
	if _, err := db.Exec("DELETE FROM messages WHERE id = 'u2'"); err != nil {
		t.Fatal(err)
	}

	pruned, err := pruneOrphanVectors(db)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM messages_vec"); got != 1 {
		t.Errorf("messages_vec rows = %d, want 1", got)
	}
	if got := count(
		t,
		db,
		"SELECT COUNT(*) FROM messages_vec_owner WHERE message_rowid = ?",
		orphan,
	); got != 0 {
		t.Errorf("orphan owner row still present")
	}
	if got := count(
		t,
		db,
		"SELECT COUNT(*) FROM embedded_messages WHERE message_rowid = ?",
		orphan,
	); got != 0 {
		t.Errorf("orphan embedding record still present")
	}
}
