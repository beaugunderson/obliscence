package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ResumeCmd struct {
	Session string `arg:"" help:"Session slug, partial slug, or UUID."`
}

func (cmd *ResumeCmd) Run(rc *RunContext) error {
	var sessionID, projectPath, sourcePath, slug string

	// Try exact match on ID or slug first.
	err := rc.DB.QueryRow(
		"SELECT id, project_path, source_path, slug FROM sessions WHERE id = ? OR slug = ?",
		cmd.Session, cmd.Session,
	).Scan(&sessionID, &projectPath, &sourcePath, &slug)

	if err == sql.ErrNoRows {
		// Try partial slug match.
		err = rc.DB.QueryRow(
			"SELECT id, project_path, source_path, slug FROM sessions WHERE slug LIKE ?",
			"%"+cmd.Session+"%",
		).Scan(&sessionID, &projectPath, &sourcePath, &slug)
	}
	if err == sql.ErrNoRows {
		return fmt.Errorf("no session matching %q", cmd.Session)
	}
	if err != nil {
		return err
	}

	// Resolve worktree paths to the actual project root.
	projectPath = resolveWorktreePath(projectPath)

	// Verify the project directory exists.
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		return fmt.Errorf("project directory no longer exists: %s", projectPath)
	}

	// Check that Claude Code will be able to find this session. The JSONL must
	// be in the project key directory that matches the cwd we'll resume from.
	// For worktree sessions the JSONL is under the worktree's project key, not
	// the main project's, so --resume won't find it.
	expectedDir := encodeProjectKey(projectPath)
	actualDir := filepath.Base(filepath.Dir(sourcePath))
	if expectedDir != actualDir {
		return fmt.Errorf(
			"session %s was created in a worktree that no longer exists\n"+
				"  use `obliscence show %s` to view the conversation instead",
			slug, slug,
		)
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found in PATH")
	}

	fmt.Fprintf(os.Stderr, "resuming %s in %s\n", dim(slug), bold(filepath.Base(projectPath)))

	c := exec.Command(claudePath, "--resume", sessionID)
	c.Dir = projectPath
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// resolveWorktreePath converts a worktree path like
// /Users/beau/p/canvas/.claude/worktrees/flapping to the project root
// /Users/beau/p/canvas.
func resolveWorktreePath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i := len(parts) - 2; i >= 1; i-- {
		if parts[i] == "worktrees" && i >= 1 && parts[i-1] == ".claude" {
			return filepath.FromSlash(strings.Join(parts[:i-1], "/"))
		}
	}
	return p
}

// encodeProjectKey encodes a directory path the same way Claude Code does:
// replace / and . with -.
func encodeProjectKey(p string) string {
	r := strings.NewReplacer("/", "-", ".", "-")
	return r.Replace(p)
}
