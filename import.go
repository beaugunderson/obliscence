package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ImportCmd ingests a claude.ai data export (the zip emailed by
// claude.ai → Settings → Privacy → Export data) into the same sessions/messages
// tables used for Claude Code, so claude.ai chats become searchable too.
//
// Idempotent: each conversation maps to a session (id = conversation uuid) and
// each chat message to a message (id = message uuid). Sessions are upserted and
// messages use INSERT OR IGNORE, so re-importing the same export — or a newer,
// overlapping one — never duplicates and only adds what's new.
type ImportCmd struct {
	Path    string `arg:"" help:"claude.ai export: a .zip, its extracted directory, or a conversations.json."`
	Force   bool   `       help:"Re-import even if this source file is unchanged."                            short:"f"`
	Verbose bool   `       help:"Show per-conversation progress."                                             short:"v"`
	NoEmbed bool   `       help:"Skip embedding generation."                                                            name:"no-embed"`
}

type claudeAIConversation struct {
	UUID      string            `json:"uuid"`
	Name      string            `json:"name"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Messages  []claudeAIMessage `json:"chat_messages"`
}

type claudeAIMessage struct {
	UUID      string `json:"uuid"`
	Text      string `json:"text"`
	Sender    string `json:"sender"`
	CreatedAt string `json:"created_at"`
	Parent    string `json:"parent_message_uuid"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (cmd *ImportCmd) Run(rc *RunContext) error {
	data, source, mtime, size, err := readClaudeExport(expandPath(cmd.Path))
	if err != nil {
		return err
	}

	// Skip an unchanged source unless forced — but still run the embed pass so a
	// prior --no-embed import gets backfilled.
	if !cmd.Force && sourceUnchanged(rc, source, mtime, size) {
		if !rc.JSON {
			fmt.Fprintf(os.Stderr, "%s already imported (unchanged); use --force to re-import\n",
				filepath.Base(source))
		}
		return cmd.maybeEmbed(rc)
	}

	var convs []claudeAIConversation
	if err := json.Unmarshal(data, &convs); err != nil {
		return fmt.Errorf("parsing conversations.json: %w", err)
	}

	tx, err := rc.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upsertSession, err := tx.Prepare(`
		INSERT INTO sessions
			(id, project_path, project_name, model, git_branch, started_at, updated_at, source_path, source_mtime, source_size, provenance)
		VALUES (?, 'claude.ai', 'claude.ai', '', '', ?, ?, ?, ?, ?, 'claude_ai')
		ON CONFLICT(id) DO UPDATE SET
			updated_at  = excluded.updated_at,
			source_path = excluded.source_path,
			source_mtime= excluded.source_mtime,
			source_size = excluded.source_size,
			provenance  = 'claude_ai'`)
	if err != nil {
		return err
	}
	defer upsertSession.Close()

	insertMsg, err := tx.Prepare(`
		INSERT OR IGNORE INTO messages
			(id, session_id, parent_id, role, content, timestamp, is_compact_summary, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, 0, NULL, NULL)`)
	if err != nil {
		return err
	}
	defer insertMsg.Close()

	var sessions, newMsgs, dupMsgs int
	for i := range convs {
		c := &convs[i]
		if c.UUID == "" {
			continue
		}
		updated := c.UpdatedAt
		if updated == "" {
			updated = c.CreatedAt
		}
		if _, err := upsertSession.Exec(
			c.UUID,
			c.CreatedAt,
			updated,
			source,
			mtime,
			size,
		); err != nil {
			return fmt.Errorf("upserting session %s: %w", c.UUID, err)
		}
		sessions++

		for j := range c.Messages {
			m := &c.Messages[j]
			content := claudeAIText(m)
			if m.UUID == "" || content == "" {
				continue
			}
			ts := m.CreatedAt
			if ts == "" {
				ts = c.CreatedAt
			}
			res, err := insertMsg.Exec(
				m.UUID,
				c.UUID,
				m.Parent,
				claudeAIRole(m.Sender),
				content,
				ts,
			)
			if err != nil {
				return fmt.Errorf("inserting message %s: %w", m.UUID, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				newMsgs++
			} else {
				dupMsgs++
			}
		}

		if cmd.Verbose {
			fmt.Fprintf(os.Stderr, "  %s (%d messages)\n", c.Name, len(c.Messages))
		}
	}

	if _, err := tx.Exec(
		"INSERT OR REPLACE INTO indexed_files (path, mtime, size) VALUES (?, ?, ?)",
		source, mtime, size,
	); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing import: %w", err)
	}

	if !rc.JSON {
		fmt.Fprintf(os.Stderr,
			"imported %d conversations: %d new messages, %d already present\n",
			sessions, newMsgs, dupMsgs)
	}

	return cmd.maybeEmbed(rc)
}

// maybeEmbed runs the shared embedding pass unless --no-embed was given.
func (cmd *ImportCmd) maybeEmbed(rc *RunContext) error {
	if cmd.NoEmbed {
		return nil
	}
	embedder, err := NewEmbedder()
	if err != nil {
		fmt.Fprintf(
			os.Stderr,
			"warning: embedder init failed: %v (FTS search works; run `obliscence index` later for semantic)\n",
			err,
		)
		return nil
	}
	if embedder == nil {
		return nil
	}
	defer embedder.Close()
	if err := embedUnembedded(rc, embedder, cmd.Verbose); err != nil {
		fmt.Fprintf(os.Stderr, "embedding error: %v\n", err)
	}
	return nil
}

// claudeAIText prefers the structured content blocks, falling back to the legacy
// flat `text` field.
func claudeAIText(m *claudeAIMessage) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return strings.TrimSpace(m.Text)
}

// claudeAIRole maps claude.ai's sender values onto obliscence's roles.
func claudeAIRole(sender string) string {
	if sender == "human" {
		return "user"
	}
	return "assistant"
}

// sourceUnchanged reports whether this exact source file has already been
// imported with the same mtime and size.
func sourceUnchanged(rc *RunContext, source string, mtime float64, size int64) bool {
	var sm float64
	var ss int64
	err := rc.DB.QueryRow(
		"SELECT mtime, size FROM indexed_files WHERE path = ?", source,
	).Scan(&sm, &ss)
	if err != nil {
		return false
	}
	return sm == mtime && ss == size
}

// readClaudeExport resolves an export path (zip, directory, or conversations.json)
// and returns the conversations.json bytes plus a stable source identity
// (path/mtime/size) used for idempotent skip tracking. For a zip, the identity is
// the zip itself; for a directory, the conversations.json inside it.
func readClaudeExport(
	path string,
) (data []byte, source string, mtime float64, size int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", 0, 0, err
	}

	if info.IsDir() {
		jsonPath := filepath.Join(path, "conversations.json")
		ji, err := os.Stat(jsonPath)
		if err != nil {
			return nil, "", 0, 0, fmt.Errorf("no conversations.json in %s: %w", path, err)
		}
		b, err := os.ReadFile(jsonPath)
		if err != nil {
			return nil, "", 0, 0, err
		}
		return b, jsonPath, statMtime(ji), ji.Size(), nil
	}

	if strings.EqualFold(filepath.Ext(path), ".zip") {
		b, err := readZipEntry(path, "conversations.json")
		if err != nil {
			return nil, "", 0, 0, err
		}
		return b, path, statMtime(info), info.Size(), nil
	}

	// Assume a conversations.json (or compatible) file.
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", 0, 0, err
	}
	return b, path, statMtime(info), info.Size(), nil
}

// readZipEntry returns the bytes of the first entry whose base name matches.
func readZipEntry(zipPath, base string) ([]byte, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for _, f := range r.File {
		if filepath.Base(f.Name) == base {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%s not found in %s", base, zipPath)
}

// statMtime returns mtime in the float seconds form used by indexed_files.
func statMtime(info os.FileInfo) float64 {
	return float64(info.ModTime().UnixMicro()) / 1e6
}
