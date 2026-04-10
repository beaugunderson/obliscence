package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type HookCmd struct{}

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
}

func (cmd *HookCmd) Run(rc *RunContext) error {
	// Always consume all stdin to avoid broken pipe errors.
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil
	}

	var input hookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil
	}

	// Try transcript path first, then find by session ID.
	// Swallow all errors — hooks must exit 0 silently to avoid
	// the false "hook error" label bug in Claude Code.
	if input.TranscriptPath != "" {
		_ = indexSingleFile(rc.DB, input.TranscriptPath)
		return nil
	}

	if input.SessionID != "" {
		projectsDir := expandPath("~/.claude/projects")

		// Try direct glob match.
		pattern := filepath.Join(projectsDir, "*", input.SessionID+".jsonl")
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			_ = indexSingleFile(rc.DB, matches[0])
			return nil
		}

		// Try encoding cwd to find the project dir.
		if input.CWD != "" {
			encoded := encodePath(input.CWD)
			path := filepath.Join(projectsDir, encoded, input.SessionID+".jsonl")
			if _, err := os.Stat(path); err == nil {
				_ = indexSingleFile(rc.DB, path)
				return nil
			}
		}
	}

	return nil
}

// indexSingleFile wraps indexFile with its own transaction for standalone use.
func indexSingleFile(db *sql.DB, path string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts, err := prepareIndexStmts(tx)
	if err != nil {
		return err
	}
	defer stmts.Close()

	if err := indexFile(tx, stmts, path); err != nil {
		return err
	}
	return tx.Commit()
}

// encodePath converts a filesystem path to Claude Code's directory encoding.
// /Users/beau/p/myproject -> -Users-beau-p-myproject
func encodePath(p string) string {
	return "-" + strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "-")
}
