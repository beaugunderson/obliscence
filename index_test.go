package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSessionRootsUnder(t *testing.T) {
	home := t.TempDir()

	// Standard Claude profile dirs, each with a projects/ subdir.
	for _, name := range []string{".claude", ".claude-personal", ".claude-teams"} {
		if err := os.MkdirAll(filepath.Join(home, name, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Pi's default session directory.
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A .claude* dir with no projects/ subdir must be excluded.
	if err := os.MkdirAll(filepath.Join(home, ".claude-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A .claude.json file (not a dir) must not match.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := sessionRootsUnder(home)
	want := []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".claude-personal", "projects"),
		filepath.Join(home, ".claude-teams", "projects"),
		filepath.Join(home, ".pi", "agent", "sessions"),
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("sessionRootsUnder()=%v, want %v", got, want)
	}
}

func TestSessionRootsHonorsPiConfigDir(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sessions", "archive"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(dir, "settings.json"),
		[]byte(`{"sessionDir":"archive"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")

	roots := sessionRoots()
	for _, name := range []string{"sessions", "archive"} {
		if want := filepath.Join(dir, name); !slices.Contains(roots, want) {
			t.Errorf("sessionRoots()=%v, want %s", roots, want)
		}
	}
}
