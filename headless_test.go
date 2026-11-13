package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeEntrypointSession writes a one-message Claude Code transcript stamped
// with the given entrypoint and returns its path.
func writeEntrypointSession(t *testing.T, dir, sid, entrypoint, text string) string {
	t.Helper()
	line := `{"type":"user","uuid":"` + sid + `-u1","parentUuid":"","sessionId":"` + sid +
		`","entrypoint":"` + entrypoint + `","cwd":"/tmp/proj","timestamp":"2026-10-01T00:00:00Z",` +
		`"message":{"role":"user","content":"` + text + `"}}`
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIndexMarksSDKSessionsHeadless(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := map[string]int{
		"11111111-0000-0000-0000-000000000001": 0, // cli
		"11111111-0000-0000-0000-000000000002": 1, // sdk-cli (claude -p)
		"11111111-0000-0000-0000-000000000003": 1, // sdk-py (Agent SDK)
	}
	entry := map[string]string{
		"11111111-0000-0000-0000-000000000001": "cli",
		"11111111-0000-0000-0000-000000000002": "sdk-cli",
		"11111111-0000-0000-0000-000000000003": "sdk-py",
	}
	for sid, ep := range entry {
		if err := indexSingleFile(
			db,
			writeEntrypointSession(t, dir, sid, ep, "hello there"),
		); err != nil {
			t.Fatal(err)
		}
	}
	for sid, want := range cases {
		if got := count(
			t,
			db,
			"SELECT COALESCE(headless, -1) FROM sessions WHERE id = ?",
			sid,
		); got != want {
			t.Errorf("%s (%s): headless = %d, want %d", sid, entry[sid], got, want)
		}
	}
}

func TestBackfillHeadless(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	interactive := "22222222-0000-0000-0000-000000000001"
	headless := "22222222-0000-0000-0000-000000000002"
	missing := "22222222-0000-0000-0000-000000000003"
	for sid, ep := range map[string]string{interactive: "cli", headless: "sdk-cli", missing: "cli"} {
		if err := indexSingleFile(
			db,
			writeEntrypointSession(t, dir, sid, ep, "hello there"),
		); err != nil {
			t.Fatal(err)
		}
	}
	// Sessions indexed before the column existed carry NULL.
	if _, err := db.Exec("UPDATE sessions SET headless = NULL"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, missing+".jsonl")); err != nil {
		t.Fatal(err)
	}

	if err := backfillHeadless(db); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{interactive: 0, headless: 1, missing: 0}
	for sid, w := range want {
		if got := count(
			t,
			db,
			"SELECT COALESCE(headless, -1) FROM sessions WHERE id = ?",
			sid,
		); got != w {
			t.Errorf("%s: headless = %d, want %d", sid, got, w)
		}
	}
}

// TestSearchExcludesHeadlessByDefault: scripted sessions (claude -p, the Agent
// SDK) carry prompts no person typed, and digests that quote other sessions'
// messages, so they only appear when --headless asks for them.
func TestSearchExcludesHeadlessByDefault(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rc := &RunContext{DB: db}

	human := "33333333-0000-0000-0000-000000000001"
	digest := "33333333-0000-0000-0000-000000000002"
	if err := indexSingleFile(
		db,
		writeEntrypointSession(t, dir, human, "cli", "what is left in this session"),
	); err != nil {
		t.Fatal(err)
	}
	if err := indexSingleFile(
		db,
		writeEntrypointSession(
			t,
			dir,
			digest,
			"sdk-cli",
			"first_msg what is left in this session",
		),
	); err != nil {
		t.Fatal(err)
	}

	for _, include := range []bool{false, true} {
		cmd := SearchCmd{
			Query:          "what is left",
			Limit:          10,
			Sort:           "relevance",
			SemanticWeight: 1.0,
			Headless:       include,
		}
		results, err := cmd.results(rc, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, r := range results {
			got[r.SessionID] = true
		}
		if !got[human] {
			t.Errorf("headless=%v: interactive session missing", include)
		}
		if got[digest] != include {
			t.Errorf("headless=%v: headless session returned = %v", include, got[digest])
		}
	}

	for _, include := range []bool{false, true} {
		cmd := CorrectionsCmd{
			Limit:    10,
			MinScore: -100,
			MaxLen:   300,
			Sort:     "score",
			Headless: include,
		}
		results, err := cmd.results(rc)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, r := range results {
			got[r.SessionID] = true
		}
		if !got[human] {
			t.Errorf("corrections headless=%v: interactive session missing", include)
		}
		if got[digest] != include {
			t.Errorf(
				"corrections headless=%v: headless session returned = %v",
				include,
				got[digest],
			)
		}
	}
}
