package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestClaudeConfigDir(t *testing.T) {
	cases := []struct {
		name       string
		sourcePath string
		want       string
	}{
		{
			name:       "main profile",
			sourcePath: "/Users/beau/.claude/projects/-Users-beau-p-obliscence/abc.jsonl",
			want:       "/Users/beau/.claude",
		},
		{
			name:       "personal profile",
			sourcePath: "/Users/beau/.claude-personal/projects/-Users-beau/def.jsonl",
			want:       "/Users/beau/.claude-personal",
		},
		{
			name:       "teams profile",
			sourcePath: "/Users/beau/.claude-teams/projects/-Users-beau/ghi.jsonl",
			want:       "/Users/beau/.claude-teams",
		},
		{
			name:       "unexpected shape returns empty",
			sourcePath: "/tmp/some/other/place.jsonl",
			want:       "",
		},
	}
	for _, c := range cases {
		if got := claudeConfigDir(c.sourcePath); got != c.want {
			t.Errorf("%s: claudeConfigDir(%q)=%q, want %q", c.name, c.sourcePath, got, c.want)
		}
	}
}

func TestResumeEnv(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(home, ".claude")
	personal := filepath.Join(home, ".claude-personal")

	// Base env carries an inherited CLAUDE_CONFIG_DIR that must be handled.
	base := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=" + personal, "TERM=xterm"}

	has := func(env []string, kv string) bool { return slices.Contains(env, kv) }
	countKey := func(env []string, key string) int {
		n := 0
		for _, kv := range env {
			if len(kv) > len(key) && kv[:len(key)+1] == key+"=" {
				n++
			}
		}
		return n
	}

	// Non-default profile: exactly one CLAUDE_CONFIG_DIR, pointing at that profile.
	t.Run("non-default profile", func(t *testing.T) {
		env := resumeEnv(base, personal)
		if !has(env, "CLAUDE_CONFIG_DIR="+personal) {
			t.Errorf("missing CLAUDE_CONFIG_DIR=%s in %v", personal, env)
		}
		if n := countKey(env, "CLAUDE_CONFIG_DIR"); n != 1 {
			t.Errorf("want exactly 1 CLAUDE_CONFIG_DIR entry, got %d: %v", n, env)
		}
		if !has(env, "PATH=/bin") {
			t.Errorf("dropped unrelated env var: %v", env)
		}
	})

	// Default profile: CLAUDE_CONFIG_DIR stripped entirely so Claude Code falls
	// back to ~/.claude and ~/.claude.json.
	t.Run("default profile strips inherited value", func(t *testing.T) {
		env := resumeEnv(base, def)
		if n := countKey(env, "CLAUDE_CONFIG_DIR"); n != 0 {
			t.Errorf("default profile must not set CLAUDE_CONFIG_DIR, got %d: %v", n, env)
		}
		if !has(env, "PATH=/bin") || !has(env, "TERM=xterm") {
			t.Errorf("dropped unrelated env vars: %v", env)
		}
	})

	// Undeterminable profile: env passed through untouched.
	t.Run("empty profile passes env through", func(t *testing.T) {
		env := resumeEnv(base, "")
		if !slices.Equal(env, base) {
			t.Errorf("empty configDir must pass env through unchanged, got %v", env)
		}
	})
}
