package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFTSQuery(t *testing.T) {
	cases := []struct {
		in    string
		exact bool
		want  string
	}{
		{"belt and suspenders", false, `"belt" "and" "suspenders"`},
		{"belt-and-suspenders", false, `"belt" "and" "suspenders"`}, // must match the spaced form
		{"rate-limiting", false, `"rate" "limiting"`},
		{"foo/bar", false, `"foo" "bar"`},
		{"what's this", false, `"what" "s" "this"`},
		{"OR", false, `"OR"`}, // FTS5 operator neutralized
		{"C++ code", false, `"C" "code"`},
		{"  ", false, ""}, // no word tokens
		{"!@#$", false, ""},
		// Exact mode wraps the tokens in a single phrase (adjacent, in order).
		{"open a PR", true, `"open a PR"`},
		{"belt-and-suspenders", true, `"belt and suspenders"`},
		{"single", true, `"single"`},
		{"  ", true, ""},
		{"!@#$", true, ""},
	}
	for _, c := range cases {
		if got := ftsQuery(c.in, c.exact); got != c.want {
			t.Errorf("ftsQuery(%q, %v) = %q, want %q", c.in, c.exact, got, c.want)
		}
	}
}

// TestSemanticSearchEndToEnd indexes a few messages, embeds them (chunked), and
// verifies semantic search ranks the relevant message first and renders the
// matching chunk as the snippet — including when the relevant text lives in the
// tail of a long message (which a fixed substr(1,500) snippet would miss).
func TestSemanticSearchEndToEnd(t *testing.T) {
	embedder := requireEmbedder(t)

	path := filepath.Join(t.TempDir(), "search.sqlite")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	rc := &RunContext{DB: db}

	if _, err := db.Exec(`INSERT INTO sessions
		(id, project_path, project_name, model, git_branch, started_at, updated_at, source_path, source_mtime, source_size)
		VALUES ('s1', '/p', 'proj', 'm', 'main', '2026-01-01', '2026-01-02', '/p/s1.jsonl', 0, 0)`); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// A long message whose only relevant content is in the tail.
	tail := "to delete files always use trash instead of rm so they can be recovered"
	longContent := strings.Repeat(
		"here is a lot of unrelated preamble about scheduling and calendars. ",
		40,
	) + tail

	msgs := []struct{ id, role, content string }{
		{"m1", "user", "what is the weather forecast for the weekend in the mountains"},
		{"m2", "assistant", "the deployment pipeline runs on aptible and ships containers"},
		{"m3", "user", longContent},
	}
	for _, m := range msgs {
		if _, err := db.Exec(
			`INSERT INTO messages (id, session_id, role, content, timestamp) VALUES (?, 's1', ?, ?, '2026-01-02')`,
			m.id,
			m.role,
			m.content,
		); err != nil {
			t.Fatalf("insert message %s: %v", m.id, err)
		}
	}

	cmd := &IndexCmd{}
	if err := cmd.embedPass(rc, embedder); err != nil {
		t.Fatalf("embedPass: %v", err)
	}

	// The long message must have produced more than one chunk.
	var chunkRows int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM messages_vec WHERE message_rowid = (SELECT rowid FROM messages WHERE id='m3')",
	).Scan(&chunkRows); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkRows < 2 {
		t.Errorf("long message produced %d chunks, want >= 2", chunkRows)
	}

	// Core #2 win: a query matching only the tail of the long message still
	// finds it. The old 256-token truncation would have dropped the tail before
	// embedding, so this message would have been unfindable by tail content.
	tailQuery, err := embedder.EmbedQuery("how do I remove files without losing them")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	results, err := (&SearchCmd{Limit: 5}).semanticResults(rc, tailQuery, 5)
	if err != nil {
		t.Fatalf("semanticResults: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if results[0].MessageID != "m3" {
		t.Errorf("tail query: top result = %s, want m3", results[0].MessageID)
	}
	// Each message appears at most once (chunk dedup).
	seen := map[string]bool{}
	for _, r := range results {
		if seen[r.MessageID] {
			t.Errorf("message %s appears more than once (dedup failed)", r.MessageID)
		}
		seen[r.MessageID] = true
	}

	// Match-centered snippet: for a single-chunk message the snippet is the
	// message text, deterministically containing the matched terms.
	weatherQuery, err := embedder.EmbedQuery("weekend weather in the hills")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	wResults, err := (&SearchCmd{Limit: 5}).semanticResults(rc, weatherQuery, 5)
	if err != nil {
		t.Fatalf("semanticResults: %v", err)
	}
	if len(wResults) == 0 || wResults[0].MessageID != "m1" {
		t.Fatalf("weather query: top result = %v, want m1", wResults)
	}
	if !strings.Contains(wResults[0].Snippet, "weather") {
		t.Errorf("snippet does not contain matched term: %q", wResults[0].Snippet)
	}
}
