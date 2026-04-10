package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type IndexCmd struct {
	Session string `help:"Index a specific session by UUID." short:"s"`
	Force   bool   `help:"Force full reindex of all sessions." short:"f"`
	Verbose bool   `help:"Show what's being indexed." short:"v"`
	NoEmbed bool   `help:"Skip embedding generation." name:"no-embed"`
}

func (cmd *IndexCmd) Run(rc *RunContext) error {
	projectsDir := expandPath("~/.claude/projects")

	// Initialize embedder if available and not disabled.
	var embedder *Embedder
	if !cmd.NoEmbed {
		var err error
		embedder, err = NewEmbedder()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: embedder init failed: %v (continuing without embeddings)\n", err)
		} else if embedder != nil {
			defer embedder.Close()
			if cmd.Verbose {
				fmt.Fprintln(os.Stderr, "embedding model loaded")
			}
		}
	}

	if cmd.Session != "" {
		return cmd.indexSession(rc, projectsDir)
	}
	return cmd.indexAll(rc, projectsDir, embedder)
}

func (cmd *IndexCmd) indexAll(rc *RunContext, projectsDir string, embedder *Embedder) error {
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

	// Collect all JSONL files and partition into needs-indexing vs skip.
	var toIndex, allFiles []string
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
			allFiles = append(allFiles, path)
			changed, err := cmd.needsIndexing(rc.DB, path)
			if err != nil {
				continue
			}
			if changed {
				toIndex = append(toIndex, path)
			}
		}
	}

	skipped := len(allFiles) - len(toIndex)
	if len(toIndex) == 0 {
		if !rc.JSON {
			fmt.Fprintf(os.Stderr, "nothing to index (%d files unchanged)\n", skipped)
		}
		return nil
	}

	// Pass 1: Text indexing (fast — makes FTS5 search available immediately).
	var indexed, errored int
	showProgress := isTTY && !cmd.Verbose && !rc.JSON
	start := time.Now()

	for i, path := range toIndex {
		if cmd.Verbose {
			fmt.Fprintf(os.Stderr, "indexing %s\n", path)
		} else if showProgress {
			printProgress("indexing", i, len(toIndex), start)
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

	if showProgress {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}
	if !rc.JSON {
		fmt.Fprintf(os.Stderr, "indexed %d, skipped %d unchanged, %d errors (%s)\n",
			indexed, skipped, errored, time.Since(start).Round(time.Millisecond))
	}

	// Pass 2: Generate embeddings for messages that don't have them yet.
	if embedder != nil {
		if err := cmd.embedPass(rc, embedder); err != nil {
			fmt.Fprintf(os.Stderr, "embedding error: %v\n", err)
		}
	}

	return nil
}

func (cmd *IndexCmd) embedPass(rc *RunContext, embedder *Embedder) error {
	// Count messages needing embeddings.
	var total int
	err := rc.DB.QueryRow(`
		SELECT COUNT(*)
		FROM messages m
		LEFT JOIN messages_vec mv ON mv.message_rowid = m.rowid
		WHERE mv.message_rowid IS NULL AND trim(m.content) != ''`,
	).Scan(&total)
	if err != nil {
		return err
	}

	if total == 0 {
		if !rc.JSON {
			fmt.Fprintln(os.Stderr, "embeddings up to date")
		}
		return nil
	}

	if !rc.JSON {
		fmt.Fprintf(os.Stderr, "generating embeddings for %d messages...\n", total)
	}

	rows, err := rc.DB.Query(`
		SELECT m.rowid, m.content
		FROM messages m
		LEFT JOIN messages_vec mv ON mv.message_rowid = m.rowid
		WHERE mv.message_rowid IS NULL AND trim(m.content) != ''`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	showProgress := isTTY && !cmd.Verbose && !rc.JSON
	start := time.Now()
	var done int

	for rows.Next() {
		var rowid int64
		var content string
		if err := rows.Scan(&rowid, &content); err != nil {
			continue
		}

		vec, err := embedder.Embed(content)
		if err != nil {
			continue
		}

		serialized, err := serializeVec(vec)
		if err != nil {
			continue
		}

		_, _ = rc.DB.Exec(
			"INSERT OR IGNORE INTO messages_vec(embedding, message_rowid) VALUES (?, ?)",
			serialized, rowid,
		)

		done++
		if showProgress {
			printProgress("embedding", done, total, start)
		}
	}

	if showProgress {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}
	if !rc.JSON {
		fmt.Fprintf(os.Stderr, "embedded %d messages (%s)\n",
			done, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func printProgress(label string, done, total int, start time.Time) {
	pct := float64(done) / float64(total)
	elapsed := time.Since(start)

	var eta string
	if done > 0 {
		perItem := elapsed / time.Duration(done)
		remaining := perItem * time.Duration(total-done)
		eta = remaining.Round(time.Second).String()
	} else {
		eta = "..."
	}

	const barWidth = 30
	filled := int(pct * barWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	fmt.Fprintf(os.Stderr, "\r\033[K%s %s %d/%d (%.0f%%) eta %s",
		label, bar, done, total, pct*100, eta)
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

	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

// embedMessages generates embeddings for messages in a session that don't have them yet.
func embedMessages(db *sql.DB, embedder *Embedder, sessionID string) {
	rows, err := db.Query(`
		SELECT m.rowid, m.content
		FROM messages m
		LEFT JOIN messages_vec mv ON mv.message_rowid = m.rowid
		WHERE m.session_id = ? AND mv.message_rowid IS NULL AND trim(m.content) != ''`,
		sessionID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var rowid int64
		var content string
		if err := rows.Scan(&rowid, &content); err != nil {
			continue
		}

		vec, err := embedder.Embed(content)
		if err != nil {
			continue
		}

		serialized, err := serializeVec(vec)
		if err != nil {
			continue
		}

		_, _ = db.Exec(
			"INSERT OR IGNORE INTO messages_vec(embedding, message_rowid) VALUES (?, ?)",
			serialized, rowid,
		)
	}
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
