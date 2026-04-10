package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type IndexCmd struct {
	Session string `help:"Index a specific session by UUID." short:"s"`
	Force   bool   `help:"Force full reindex of all sessions." short:"f"`
	Verbose bool   `help:"Show what's being indexed." short:"v"`
}

func (cmd *IndexCmd) Run(rc *RunContext) error {
	projectsDir := expandPath("~/.claude/projects")

	if cmd.Session != "" {
		return cmd.indexSession(rc, projectsDir)
	}
	return cmd.indexAll(rc, projectsDir)
}

func (cmd *IndexCmd) indexAll(rc *RunContext, projectsDir string) error {
	if cmd.Force {
		if _, err := rc.DB.Exec("DELETE FROM indexed_files"); err != nil {
			return fmt.Errorf("clearing index state: %w", err)
		}
		if cmd.Verbose {
			fmt.Fprintln(os.Stderr, "cleared index state, forcing full reindex")
		}
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return fmt.Errorf("reading projects dir: %w", err)
	}

	var indexed, skipped, errored int

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dirPath := filepath.Join(projectsDir, entry.Name())
		jsonlFiles, err := filepath.Glob(filepath.Join(dirPath, "*.jsonl"))
		if err != nil {
			continue
		}

		for _, path := range jsonlFiles {
			changed, err := cmd.needsIndexing(rc.DB, path)
			if err != nil {
				if cmd.Verbose {
					fmt.Fprintf(os.Stderr, "error checking %s: %v\n", path, err)
				}
				errored++
				continue
			}
			if !changed {
				skipped++
				continue
			}

			if cmd.Verbose {
				fmt.Fprintf(os.Stderr, "indexing %s\n", path)
			}

			if err := indexFile(rc.DB, path); err != nil {
				if cmd.Verbose {
					fmt.Fprintf(os.Stderr, "error indexing %s: %v\n", path, err)
				}
				errored++
				continue
			}
			indexed++
		}
	}

	if cmd.Verbose || !rc.JSON {
		fmt.Fprintf(os.Stderr, "indexed %d, skipped %d unchanged, %d errors\n", indexed, skipped, errored)
	}
	return nil
}

func (cmd *IndexCmd) indexSession(rc *RunContext, projectsDir string) error {
	// Find the JSONL file for this session UUID.
	pattern := filepath.Join(projectsDir, "*", cmd.Session+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("session %s not found", cmd.Session)
	}
	return indexFile(rc.DB, matches[0])
}

func (cmd *IndexCmd) needsIndexing(db *sql.DB, path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	var storedMtime float64
	var storedSize int64
	err = db.QueryRow(
		"SELECT mtime, size FROM indexed_files WHERE path = ?",
		path,
	).Scan(&storedMtime, &storedSize)

	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	mtime := float64(info.ModTime().UnixMicro()) / 1e6
	return mtime != storedMtime || info.Size() != storedSize, nil
}

// indexFile parses a JSONL file and upserts its contents into the database.
func indexFile(db *sql.DB, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sess sessionMeta
	var messages []parsedMessage
	var tools []parsedToolUse

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}

		msgType := unquote(raw["type"])

		// Extract session metadata from any message that has it.
		if sess.id == "" {
			extractSessionMeta(&sess, raw)
		}
		// Fill in cwd from later messages if the first one didn't have it.
		if sess.projectPath == "" {
			if cwd := unquote(raw["cwd"]); cwd != "" {
				sess.projectPath, sess.projectName = resolveProject(cwd)
			}
		}
		// Update slug/branch from later messages if present.
		if s := unquote(raw["slug"]); s != "" {
			sess.slug = s
		}
		if s := unquote(raw["gitBranch"]); s != "" {
			sess.gitBranch = s
		}

		switch msgType {
		case "user":
			msg := parseUserMessage(raw)
			if msg != nil {
				messages = append(messages, *msg)
				if ts := msg.timestamp; ts != "" {
					sess.updatedAt = ts
					if sess.startedAt == "" {
						sess.startedAt = ts
					}
				}
			}
		case "assistant":
			msg, toolUses := parseAssistantMessage(raw)
			if msg != nil {
				messages = append(messages, *msg)
				tools = append(tools, toolUses...)
				if sess.model == "" {
					sess.model = msg.model
				}
				if ts := msg.timestamp; ts != "" {
					sess.updatedAt = ts
				}
			}
		}
	}

	mtime := float64(info.ModTime().UnixMicro()) / 1e6

	if sess.id == "" {
		// No session found, but record the file so we skip it next time.
		_, _ = tx.Exec(
			"INSERT OR REPLACE INTO indexed_files (path, mtime, size) VALUES (?, ?, ?)",
			path, mtime, info.Size(),
		)
		return tx.Commit()
	}

	// Fallback: derive project from the parent directory name in the file path.
	// The directory name is an encoded path like -Users-beau-p-myproject.
	// Since decoding is ambiguous (hyphens in project names), use cwd from
	// messages when available. This fallback only fires when no message had cwd.
	if sess.projectPath == "" {
		dirName := filepath.Base(filepath.Dir(path))
		sess.projectPath = dirName
		sess.projectName = dirName
	}

	// Delete existing data for this session (full re-index on change).
	_, _ = tx.Exec("DELETE FROM sessions WHERE id = ?", sess.id)

	_, err = tx.Exec(`
		INSERT INTO sessions (id, project_path, project_name, slug, model, git_branch, started_at, updated_at, source_path, source_mtime, source_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.id, sess.projectPath, sess.projectName, sess.slug, sess.model, sess.gitBranch,
		sess.startedAt, sess.updatedAt, path, mtime, info.Size(),
	)
	if err != nil {
		return fmt.Errorf("inserting session: %w", err)
	}

	msgStmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO messages (id, session_id, parent_id, role, content, timestamp, is_compact_summary, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer msgStmt.Close()

	for _, m := range messages {
		_, err = msgStmt.Exec(m.id, sess.id, m.parentID, m.role, m.content, m.timestamp, m.isCompactSummary, m.inputTokens, m.outputTokens)
		if err != nil {
			return fmt.Errorf("inserting message: %w", err)
		}
	}

	toolStmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO tool_uses (id, message_id, session_id, tool_name, tool_input_summary)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer toolStmt.Close()

	for _, t := range tools {
		_, err = toolStmt.Exec(t.id, t.messageID, sess.id, t.toolName, t.inputSummary)
		if err != nil {
			return fmt.Errorf("inserting tool_use: %w", err)
		}
	}

	// Record file as indexed.
	_, err = tx.Exec(
		"INSERT OR REPLACE INTO indexed_files (path, mtime, size) VALUES (?, ?, ?)",
		path, mtime, info.Size(),
	)
	if err != nil {
		return fmt.Errorf("recording indexed file: %w", err)
	}

	return tx.Commit()
}

type sessionMeta struct {
	id          string
	projectPath string
	projectName string
	slug        string
	model       string
	gitBranch   string
	startedAt   string
	updatedAt   string
}

type parsedMessage struct {
	id               string
	parentID         string
	role             string
	content          string
	timestamp        string
	model            string
	isCompactSummary bool
	inputTokens      int
	outputTokens     int
}

type parsedToolUse struct {
	id           string
	messageID    string
	toolName     string
	inputSummary string
}

func extractSessionMeta(sess *sessionMeta, raw map[string]json.RawMessage) {
	sess.id = unquote(raw["sessionId"])
	if sess.id == "" {
		return
	}

	cwd := unquote(raw["cwd"])
	if cwd != "" {
		sess.projectPath, sess.projectName = resolveProject(cwd)
	}

	sess.slug = unquote(raw["slug"])
	sess.gitBranch = unquote(raw["gitBranch"])
}

// resolveProject returns (projectPath, projectName) for a cwd, handling
// Claude Code worktree directories like .claude/worktrees/<slug> by resolving
// to the parent project.
func resolveProject(cwd string) (string, string) {
	// Detect .claude/worktrees/<name> pattern and resolve to parent project.
	parts := strings.Split(filepath.ToSlash(cwd), "/")
	for i := len(parts) - 2; i >= 1; i-- {
		if parts[i] == "worktrees" && i >= 1 && parts[i-1] == ".claude" {
			// The project root is everything before .claude/
			projectPath := strings.Join(parts[:i-1], "/")
			return projectPath, filepath.Base(projectPath)
		}
	}
	return cwd, filepath.Base(cwd)
}

func parseUserMessage(raw map[string]json.RawMessage) *parsedMessage {
	// Skip meta/system messages.
	if unquote(raw["isMeta"]) == "true" {
		return nil
	}

	content := extractContent(raw["message"])
	if content == "" {
		return nil
	}

	return &parsedMessage{
		id:               unquote(raw["uuid"]),
		parentID:         unquote(raw["parentUuid"]),
		role:             "user",
		content:          content,
		timestamp:        unquote(raw["timestamp"]),
		isCompactSummary: unquote(raw["isCompactSummary"]) == "true",
	}
}

func parseAssistantMessage(raw map[string]json.RawMessage) (*parsedMessage, []parsedToolUse) {
	var msgObj struct {
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content json.RawMessage
		Usage   struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(raw["message"], &msgObj); err != nil {
		return nil, nil
	}

	msgID := unquote(raw["uuid"])
	var textParts []string
	var tools []parsedToolUse

	// Content can be a string or an array of content blocks.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msgObj.Content, &blocks); err != nil {
		// Try as string.
		var s string
		if err := json.Unmarshal(msgObj.Content, &s); err == nil && s != "" {
			textParts = append(textParts, s)
		}
	} else {
		for _, block := range blocks {
			blockType := unquote(block["type"])
			switch blockType {
			case "text":
				if t := unquote(block["text"]); t != "" {
					textParts = append(textParts, t)
				}
			case "tool_use":
				tu := parsedToolUse{
					id:        unquote(block["id"]),
					messageID: msgID,
					toolName:  unquote(block["name"]),
				}
				tu.inputSummary = extractToolSummary(tu.toolName, block["input"])
				tools = append(tools, tu)
			}
		}
	}

	content := strings.Join(textParts, "\n")
	if content == "" && len(tools) == 0 {
		return nil, nil
	}

	msg := &parsedMessage{
		id:           msgID,
		parentID:     unquote(raw["parentUuid"]),
		role:         "assistant",
		content:      content,
		timestamp:    unquote(raw["timestamp"]),
		model:        msgObj.Model,
		inputTokens:  msgObj.Usage.InputTokens,
		outputTokens: msgObj.Usage.OutputTokens,
	}

	return msg, tools
}

// extractContent gets text from a message object's content field.
// Content can be a string or an array of content blocks.
func extractContent(msgRaw json.RawMessage) string {
	if msgRaw == nil {
		return ""
	}

	var msgObj struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgRaw, &msgObj); err != nil {
		return ""
	}

	// Try as string first.
	var s string
	if err := json.Unmarshal(msgObj.Content, &s); err == nil {
		return s
	}

	// Try as array of content blocks.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msgObj.Content, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, block := range blocks {
		blockType := unquote(block["type"])
		switch blockType {
		case "text":
			if t := unquote(block["text"]); t != "" {
				parts = append(parts, t)
			}
		case "tool_result":
			// Extract text from tool_result content if it's a string.
			if t := unquote(block["content"]); t != "" {
				// Skip tool results — they're tool output, not user text.
			}
		}
	}

	return strings.Join(parts, "\n")
}

// extractToolSummary pulls the most useful field from a tool's input.
func extractToolSummary(toolName string, inputRaw json.RawMessage) string {
	if inputRaw == nil {
		return ""
	}

	var input map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return ""
	}

	switch toolName {
	case "Bash":
		return unquote(input["command"])
	case "Read", "Write", "Edit":
		return unquote(input["file_path"])
	case "Grep":
		return unquote(input["pattern"])
	case "Glob":
		return unquote(input["pattern"])
	case "Agent":
		return unquote(input["description"])
	default:
		return ""
	}
}

// unquote extracts a JSON string value, returning "" for non-strings or errors.
func unquote(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
