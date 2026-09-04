package main

import (
	"fmt"
	"path/filepath"
	"sort"
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

// testSession and testMessage describe a fixture corpus for the search tests.
type testSession struct{ id, project, provenance string }

type testMessage struct{ id, session, role, timestamp, content string }

// newSearchCorpus builds a temp DB from the given fixtures and embeds every
// message, so keyword, semantic, and hybrid search all have something to find.
func newSearchCorpus(
	t *testing.T,
	embedder *Embedder,
	sessions []testSession,
	messages []testMessage,
) *RunContext {
	t.Helper()

	db, err := openDB(filepath.Join(t.TempDir(), "search.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rc := &RunContext{DB: db}

	for _, s := range sessions {
		prov := s.provenance
		if prov == "" {
			prov = "claude_code"
		}
		if _, err := db.Exec(`INSERT INTO sessions
			(id, project_path, project_name, provenance, model, git_branch,
			 started_at, updated_at, source_path, source_mtime, source_size)
			VALUES (?, '/p', ?, ?, 'm', 'main', '2026-01-01', '2026-01-02', ?, 0, 0)`,
			s.id, s.project, prov, "/p/"+s.id+".jsonl",
		); err != nil {
			t.Fatalf("insert session %s: %v", s.id, err)
		}
	}
	for _, m := range messages {
		if _, err := db.Exec(
			`INSERT INTO messages (id, session_id, role, content, timestamp) VALUES (?, ?, ?, ?, ?)`,
			m.id,
			m.session,
			m.role,
			m.content,
			m.timestamp,
		); err != nil {
			t.Fatalf("insert message %s: %v", m.id, err)
		}
	}
	if err := (&IndexCmd{}).embedPass(rc, embedder); err != nil {
		t.Fatalf("embedPass: %v", err)
	}
	return rc
}

// trashCorpus is a corpus where every message matches the query both lexically
// (all share the tokens "delete", "files", "trash") and semantically, split
// across two projects, two roles, and two date ranges. Any scope flag that a
// retrieval branch drops therefore shows up as an out-of-scope hit.
func trashCorpus(t *testing.T, embedder *Embedder) *RunContext {
	t.Helper()
	return newSearchCorpus(t, embedder,
		[]testSession{
			{id: "s_old", project: "oldproj"},
			{id: "s_new", project: "newproj"},
		},
		[]testMessage{
			{
				"m_old_user", "s_old", "user", "2026-01-05T10:00:00.000Z",
				"when I delete files I want them moved to the trash so they can be restored later",
			},
			{
				"m_old_asst", "s_old", "assistant", "2026-01-05T10:01:00.000Z",
				"use trash to delete files instead of rm, since rm cannot be undone afterwards",
			},
			{
				"m_new_user", "s_new", "user", "2026-07-20T10:00:00.000Z",
				"reminder: delete files by sending them to the trash, never reach for rm",
			},
			{
				"m_new_asst", "s_new", "assistant", "2026-07-20T10:01:00.000Z",
				"confirmed, delete files with trash so that recovery stays possible",
			},
		},
	)
}

const trashQuery = "delete files trash"

// TestFiltersConstrainEveryRetrievalMode asserts that each scope flag holds in
// keyword, semantic, and hybrid mode. Hybrid fuses two rankings, so a filter
// applied to only one branch leaks out-of-scope rows into the fused output.
func TestFiltersConstrainEveryRetrievalMode(t *testing.T) {
	embedder := requireEmbedder(t)
	rc := trashCorpus(t, embedder)

	cases := []struct {
		name string
		cmd  SearchCmd
		want []string // message IDs, any order
	}{
		{"after", SearchCmd{After: "2026-07-18"}, []string{"m_new_user", "m_new_asst"}},
		{"before", SearchCmd{Before: "2026-07-18"}, []string{"m_old_user", "m_old_asst"}},
		{"project", SearchCmd{Project: "newproj"}, []string{"m_new_user", "m_new_asst"}},
		{"role", SearchCmd{Role: "user"}, []string{"m_old_user", "m_new_user"}},
		{
			"after and role",
			SearchCmd{After: "2026-07-18", Role: "assistant"},
			[]string{"m_new_asst"},
		},
	}

	modes := []struct {
		name  string
		apply func(*SearchCmd)
	}{
		{"keyword", func(c *SearchCmd) {}},
		{"semantic", func(c *SearchCmd) { c.Semantic = true }},
		{"hybrid", func(c *SearchCmd) { c.Hybrid = true }},
	}

	for _, mode := range modes {
		for _, c := range cases {
			t.Run(mode.name+"/"+c.name, func(t *testing.T) {
				cmd := c.cmd
				cmd.Query = trashQuery
				cmd.Limit = 10
				cmd.Sort = "relevance"
				cmd.SemanticWeight = 1.0
				mode.apply(&cmd)

				results, err := cmd.results(rc, embedder)
				if err != nil {
					t.Fatalf("results: %v", err)
				}

				got := map[string]bool{}
				for _, r := range results {
					got[r.MessageID] = true
				}
				for _, id := range c.want {
					if !got[id] {
						t.Errorf("missing in-scope hit %s (got %v)", id, keys(got))
					}
					delete(got, id)
				}
				if len(got) > 0 {
					t.Errorf("returned out-of-scope hits %v; filter was dropped", keys(got))
				}
			})
		}
	}
}

// TestFiltersConstrainCandidateSetNotResults pins the filter to the retrieval
// step rather than the results. When the nearest neighbors are all out of scope,
// a post-filter discards them and returns nothing; filtering the candidate set
// spends the top-k on rows that can actually be returned.
func TestFiltersConstrainCandidateSetNotResults(t *testing.T) {
	embedder := requireEmbedder(t)

	// Five out-of-window messages that restate the query almost verbatim, so
	// they own every nearest-neighbor slot, plus one in-window message on an
	// unrelated topic. It is the only admissible hit, and a date-bounded search
	// must return it.
	messages := []testMessage{
		{
			"m_in_window", "s_new", "user", "2026-07-20T10:00:00.000Z",
			"remember to always use trash instead of rm when deleting files",
		},
	}
	near := []string{
		"the deployment pipeline ships containers to aptible",
		"our deployment pipeline ships containers to aptible on merge",
		"the deployment pipeline ships the containers to aptible each release",
		"deployment pipeline ships containers to aptible automatically",
		"containers are shipped to aptible by the deployment pipeline",
	}
	for i, content := range near {
		messages = append(messages, testMessage{
			id:        fmt.Sprintf("m_old_%d", i),
			session:   "s_old",
			role:      "user",
			timestamp: fmt.Sprintf("2026-01-0%dT10:00:00.000Z", i+1),
			content:   content,
		})
	}

	rc := newSearchCorpus(t, embedder,
		[]testSession{{id: "s_old", project: "oldproj"}, {id: "s_new", project: "newproj"}},
		messages,
	)

	cmd := SearchCmd{
		Query:          "the deployment pipeline ships containers to aptible",
		Limit:          1,
		Sort:           "relevance",
		SemanticWeight: 1.0,
		Semantic:       true,
		After:          "2026-07-18",
	}
	results, err := cmd.results(rc, embedder)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 1 || results[0].MessageID != "m_in_window" {
		t.Errorf(
			"date-bounded semantic search returned %d hits (%v), want just m_in_window; "+
				"out-of-window neighbors consumed the top-k",
			len(results), ids(results),
		)
	}
}

func TestNormalizeProvenance(t *testing.T) {
	cases := map[string]string{
		"claude.ai":   "claude_ai",
		"claude-code": "claude_code",
		"local":       "claude_code",
		"pi":          "pi",
	}
	for input, want := range cases {
		got, err := normalizeProvenance(input)
		if err != nil || got != want {
			t.Errorf("normalizeProvenance(%q)=(%q, %v), want %q", input, got, err, want)
		}
	}
	if _, err := normalizeProvenance(
		"cursor",
	); err == nil ||
		!strings.Contains(err.Error(), "pi") {
		t.Errorf("invalid source error = %v, want valid options including pi", err)
	}
}

// TestSearchRejectsUnhonorableFlags covers flags a mode cannot act on. Each must
// be an error, since ignoring one silently answers a different question.
func TestSearchRejectsUnhonorableFlags(t *testing.T) {
	cases := []struct {
		name string
		cmd  SearchCmd
		want string
	}{
		{
			"exact with semantic",
			SearchCmd{Exact: true, Semantic: true, SemanticWeight: 1.0},
			"--exact",
		},
		{
			"semantic weight without hybrid",
			SearchCmd{SemanticWeight: 2.0},
			"--semantic-weight",
		},
		{
			"semantic weight with semantic",
			SearchCmd{Semantic: true, SemanticWeight: 2.0},
			"--semantic-weight",
		},
		{
			"semantic and hybrid together",
			SearchCmd{Semantic: true, Hybrid: true, SemanticWeight: 1.0},
			"mutually exclusive",
		},
		{
			"zero semantic weight",
			SearchCmd{Hybrid: true, SemanticWeight: 0},
			"greater than 0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cmd.validate()
			if err == nil {
				t.Fatalf("validate() = nil, want an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("validate() = %q, want it to mention %q", err, c.want)
			}
		})
	}

	// Combinations every mode can honor stay valid.
	valid := []SearchCmd{
		{Exact: true, SemanticWeight: 1.0},
		{Exact: true, Hybrid: true, SemanticWeight: 1.0},
		{Hybrid: true, SemanticWeight: 2.0},
		{Semantic: true, SemanticWeight: 1.0},
	}
	for i, cmd := range valid {
		if err := cmd.validate(); err != nil {
			t.Errorf("valid[%d] %+v: validate() = %v, want nil", i, cmd, err)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ids(results []SearchResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.MessageID)
	}
	return out
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
	results, err := (&SearchCmd{Limit: 5}).vectorResults(rc, tailQuery, 5)
	if err != nil {
		t.Fatalf("vectorResults: %v", err)
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
	wResults, err := (&SearchCmd{Limit: 5}).vectorResults(rc, weatherQuery, 5)
	if err != nil {
		t.Fatalf("vectorResults: %v", err)
	}
	if len(wResults) == 0 || wResults[0].MessageID != "m1" {
		t.Fatalf("weather query: top result = %v, want m1", wResults)
	}
	if !strings.Contains(wResults[0].Snippet, "weather") {
		t.Errorf("snippet does not contain matched term: %q", wResults[0].Snippet)
	}
}
