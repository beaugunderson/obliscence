package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestClaudeRootsUnder(t *testing.T) {
	home := t.TempDir()

	// Standard profile dirs, each with a projects/ subdir.
	for _, name := range []string{".claude", ".claude-personal", ".claude-teams"} {
		if err := os.MkdirAll(filepath.Join(home, name, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A .claude* dir with no projects/ subdir must be excluded.
	if err := os.MkdirAll(filepath.Join(home, ".claude-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A .claude.json file (not a dir) must not match.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := claudeRootsUnder(home)
	want := []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".claude-personal", "projects"),
		filepath.Join(home, ".claude-teams", "projects"),
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("claudeRootsUnder()=%v, want %v", got, want)
	}
}
