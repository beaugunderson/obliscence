package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func projectCorpus(t *testing.T) *RunContext {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "project.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, name := range []string{"singing-bowls", "personal-wiki"} {
		if _, err := db.Exec(`INSERT INTO sessions
			(id, project_path, project_name, provenance, model, git_branch,
			 started_at, updated_at, source_path, source_mtime, source_size)
			VALUES (?, '/p', ?, 'claude_code', 'm', 'main', '2026-01-01', '2026-01-02', ?, 0, 0)`,
			"s_"+name, name, "/p/"+name+".jsonl",
		); err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}
	return &RunContext{DB: db}
}

// An unmatched --project used to return an empty result, which reads as "no hits in
// that project" rather than "that project name is wrong".
func TestUnknownProjectIsAnError(t *testing.T) {
	rc := projectCorpus(t)

	err := checkProject(rc.DB, "p-singing-bowls")
	if err == nil {
		t.Fatal("checkProject(p-singing-bowls) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "singing-bowls") {
		t.Errorf("error %q should suggest the project the name contains", err)
	}

	for _, ok := range []string{"", "singing-bowls", "singing", "wiki"} {
		if err := checkProject(rc.DB, ok); err != nil {
			t.Errorf("checkProject(%q) = %v, want nil", ok, err)
		}
	}

	if err := (&SessionsCmd{Project: "nope", Limit: 5}).Run(rc); err == nil {
		t.Error("sessions --project nope: want an error")
	}
	if err := (&CorrectionsCmd{Project: "nope", Limit: 5}).Run(rc); err == nil {
		t.Error("corrections --project nope: want an error")
	}
	if err := (&SearchCmd{Project: "nope", Query: "x", Limit: 5, SemanticWeight: 1.0}).Run(
		rc,
	); err == nil {
		t.Error("search --project nope: want an error")
	}
}
