package main

import "testing"

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
