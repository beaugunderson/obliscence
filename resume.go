package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ResumeCmd struct {
	Session string `arg:"" help:"Session UUID (full or unique prefix)."`
}

func (cmd *ResumeCmd) Run(rc *RunContext) error {
	sessionID, err := resolveSessionID(rc, cmd.Session)
	if err != nil {
		return err
	}

	var projectPath, sourcePath, provenance string
	err = rc.DB.QueryRow(
		"SELECT project_path, source_path, provenance FROM sessions WHERE id = ?",
		sessionID,
	).Scan(&projectPath, &sourcePath, &provenance)
	if err != nil {
		return err
	}

	if provenance == "pi" {
		return resumePi(sessionID, projectPath, sourcePath)
	}
	if provenance == "claude_ai" {
		return fmt.Errorf("claude.ai web sessions cannot be resumed")
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
			sessionID, sessionID,
		)
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found in PATH")
	}

	// A session lives under the Claude profile whose projects/ dir holds its
	// transcript (~/.claude, ~/.claude-personal, ...). Point Claude Code at that
	// profile via CLAUDE_CONFIG_DIR so --resume finds the session, regardless of
	// which profile the current shell defaults to.
	configDir := claudeConfigDir(sourcePath)

	if configDir != "" && configDir != defaultConfigDir() {
		fmt.Fprintf(os.Stderr, "resuming %s in %s (%s)\n",
			dim(sessionID), bold(filepath.Base(projectPath)), dim(filepath.Base(configDir)))
	} else {
		fmt.Fprintf(
			os.Stderr,
			"resuming %s in %s\n",
			dim(sessionID),
			bold(filepath.Base(projectPath)),
		)
	}

	c := exec.Command(claudePath, "--resume", sessionID)
	c.Dir = projectPath
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = resumeEnv(os.Environ(), configDir)
	return c.Run()
}

func resumePi(sessionID, projectPath, sourcePath string) error {
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		return fmt.Errorf("project directory no longer exists: %s", projectPath)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("pi session file no longer exists: %s", sourcePath)
	}
	piPath, err := exec.LookPath("pi")
	if err != nil {
		return fmt.Errorf("pi not found in PATH")
	}

	fmt.Fprintf(os.Stderr, "resuming %s in %s with pi\n",
		dim(sessionID), bold(filepath.Base(projectPath)))
	c := exec.Command(piPath, "--session", sourcePath)
	c.Dir = projectPath
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// resumeEnv builds the environment for the spawned `claude --resume`. A
// non-default profile is injected as CLAUDE_CONFIG_DIR. The default profile
// (~/.claude) instead strips any inherited CLAUDE_CONFIG_DIR so Claude Code
// falls back to ~/.claude and its ~/.claude.json machine state at the home
// root — setting CLAUDE_CONFIG_DIR=~/.claude would make it read
// ~/.claude/.claude.json instead. An undeterminable profile leaves the
// inherited environment untouched.
func resumeEnv(base []string, configDir string) []string {
	if configDir == "" {
		return base
	}
	stripped := removeEnv(base, "CLAUDE_CONFIG_DIR")
	if configDir == defaultConfigDir() {
		return stripped
	}
	return append(stripped, "CLAUDE_CONFIG_DIR="+configDir)
}

// removeEnv returns env with any "key=..." entry removed.
func removeEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// claudeConfigDir derives the Claude Code config directory that owns a session
// transcript from its source path. Transcripts live at
// <configDir>/projects/<projectKey>/<uuid>.jsonl, so the config dir is three
// levels up. Returns "" if the path doesn't have that shape.
func claudeConfigDir(sourcePath string) string {
	projectsDir := filepath.Dir(filepath.Dir(sourcePath))
	if filepath.Base(projectsDir) != "projects" {
		return ""
	}
	return filepath.Dir(projectsDir)
}

// defaultConfigDir returns ~/.claude, the profile Claude Code uses when
// CLAUDE_CONFIG_DIR is unset.
func defaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
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
