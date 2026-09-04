package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type IndexCmd struct {
	Session string `help:"Index a specific session by UUID."   short:"s"`
	Force   bool   `help:"Force full reindex of all sessions." short:"f"`
	Verbose bool   `help:"Show what's being indexed."          short:"v"`
	NoEmbed bool   `help:"Skip embedding generation."                    name:"no-embed"`
}

// sessionRoots returns every directory whose immediate children hold session
// transcripts. Claude Code keeps profiles under ~/.claude*/projects; pi keeps
// them under ~/.pi/agent/sessions by default.
func sessionRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	roots := sessionRootsUnder(home)

	// Pi allows its config and session directories to be moved. Honor those
	// overrides in addition to the default so old and current stores can coexist.
	piDir := expandPath(os.Getenv("PI_CODING_AGENT_DIR"))
	if piDir == "" {
		piDir = filepath.Join(home, ".pi", "agent")
	}
	roots = appendExistingDir(roots, filepath.Join(piDir, "sessions"))
	if envDir := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); envDir != "" {
		roots = appendExistingDir(roots, expandPath(envDir))
	} else if data, err := os.ReadFile(filepath.Join(piDir, "settings.json")); err == nil {
		var settings struct {
			SessionDir string `json:"sessionDir"`
		}
		if json.Unmarshal(data, &settings) == nil && settings.SessionDir != "" {
			dir := expandPath(settings.SessionDir)
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(piDir, dir)
			}
			roots = appendExistingDir(roots, dir)
		}
	}
	return roots
}

// claudeRoots is retained for hook lookup, which only handles Claude's hook
// payload when no transcript path is supplied.
func claudeRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return claudeRootsUnder(home)
}

// sessionRootsUnder returns the default Claude Code and pi transcript roots.
// Split from sessionRoots so tests can point it at a temporary home.
func sessionRootsUnder(home string) []string {
	roots := claudeRootsUnder(home)
	return appendExistingDir(roots, filepath.Join(home, ".pi", "agent", "sessions"))
}

// claudeRootsUnder globs home for ".claude*/projects" directories, returning
// only those that exist as directories.
func claudeRootsUnder(home string) []string {
	matches, _ := filepath.Glob(filepath.Join(home, ".claude*", "projects"))
	var roots []string
	for _, m := range matches {
		roots = appendExistingDir(roots, m)
	}
	return roots
}

func appendExistingDir(roots []string, dir string) []string {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return roots
	}
	for _, root := range roots {
		if root == dir {
			return roots
		}
	}
	return append(roots, dir)
}

func (cmd *IndexCmd) Run(rc *RunContext) error {
	roots := sessionRoots()
	if len(roots) == 0 {
		return fmt.Errorf("no Claude Code or pi session directories found")
	}

	// Initialize embedder if available and not disabled.
	var embedder *Embedder
	if !cmd.NoEmbed {
		var err error
		embedder, err = NewEmbedder()
		if err != nil {
			fmt.Fprintf(
				os.Stderr,
				"warning: embedder init failed: %v (continuing without embeddings)\n",
				err,
			)
		} else if embedder != nil {
			defer embedder.Close()
			if cmd.Verbose {
				fmt.Fprintln(os.Stderr, "embedding model loaded")
			}
		}
	}

	if cmd.Session != "" {
		return cmd.indexSession(rc, roots)
	}
	return cmd.indexAll(rc, roots, embedder)
}

func (cmd *IndexCmd) indexAll(rc *RunContext, roots []string, embedder *Embedder) error {
	if cmd.Force {
		if _, err := rc.DB.Exec("DELETE FROM indexed_files"); err != nil {
			return fmt.Errorf("clearing index state: %w", err)
		}
		if cmd.Verbose {
			fmt.Fprintln(os.Stderr, "cleared index state, forcing full reindex")
		}
	}

	pruned, err := pruneOrphanVectors(rc.DB)
	if err != nil {
		return fmt.Errorf("pruning orphan vectors: %w", err)
	}
	if pruned > 0 && cmd.Verbose {
		fmt.Fprintf(os.Stderr, "pruned %d orphan vectors\n", pruned)
	}

	// Load all indexed file states into memory to avoid per-file DB queries.
	indexedFiles, err := loadIndexedFiles(rc.DB)
	if err != nil {
		return fmt.Errorf("loading index state: %w", err)
	}

	// Collect all JSONL files and check which need indexing.
	// Parallelize directory scanning since os.Stat is the bottleneck.
	type scanResult struct {
		all     []string
		changed []string
	}

	// Gather the project subdirectories across every Claude root.
	var dirs []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Errorf("reading projects dir %s: %w", root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(root, entry.Name()))
			}
		}
	}

	results := make([]scanResult, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			var r scanResult
			for _, path := range sessionFiles(dir) {
				r.all = append(r.all, path)
				if needsIndexing(indexedFiles, path) {
					r.changed = append(r.changed, path)
				}
			}
			results[i] = r
		}(i, dir)
	}
	wg.Wait()

	var toIndex, allFiles []string
	for _, r := range results {
		allFiles = append(allFiles, r.all...)
		toIndex = append(toIndex, r.changed...)
	}

	skipped := len(allFiles) - len(toIndex)
	if len(toIndex) == 0 {
		if !rc.JSON {
			fmt.Fprintf(os.Stderr, "nothing to index (%d files unchanged)\n", skipped)
		}
		// Still run embedding pass — there may be un-embedded messages from a prior --no-embed run.
		if embedder != nil {
			if err := cmd.embedPass(rc, embedder); err != nil {
				fmt.Fprintf(os.Stderr, "embedding error: %v\n", err)
			}
		}
		return nil
	}

	// Pass 1: Text indexing (fast — makes FTS5 search available immediately).
	// Use a single transaction with pre-prepared statements for all files.
	// For bulk indexing (>10 files), disable FTS triggers and rebuild after.
	bulkMode := len(toIndex) > 10
	var indexed, errored int
	showProgress := isTTY && !cmd.Verbose && !rc.JSON
	start := time.Now()

	tx, err := rc.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if bulkMode {
		tx.Exec("DROP TRIGGER IF EXISTS messages_ai")
		tx.Exec("DROP TRIGGER IF EXISTS messages_ad")
		tx.Exec("DROP TRIGGER IF EXISTS messages_au")
	}

	stmts, err := prepareIndexStmts(tx)
	if err != nil {
		return err
	}
	defer stmts.Close()

	for i, path := range toIndex {
		if cmd.Verbose {
			fmt.Fprintf(os.Stderr, "indexing %s\n", path)
		} else if showProgress {
			printProgress("indexing", i, len(toIndex), start)
		}

		if err := indexFile(tx, stmts, path); err != nil {
			if cmd.Verbose {
				fmt.Fprintf(os.Stderr, "error indexing %s: %v\n", path, err)
			}
			errored++
			continue
		}
		indexed++
	}

	if bulkMode {
		// Rebuild FTS index from scratch.
		tx.Exec("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')")
		// Recreate triggers for future incremental updates.
		tx.Exec(`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
		END`)
		tx.Exec(`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
		END`)
		tx.Exec(`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
		END`)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing index: %w", err)
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
	return embedUnembedded(rc, embedder, cmd.Verbose)
}

// embedUnembedded generates embeddings for any message that lacks them,
// regardless of source. Shared by `index` and `import`.
func embedUnembedded(rc *RunContext, embedder *Embedder, verbose bool) error {
	// Count messages needing embeddings.
	// Use embedded_messages tracking table instead of LEFT JOIN on vec0 virtual table
	// (vec0 JOINs are extremely slow).
	var total int
	err := rc.DB.QueryRow(`
		SELECT COUNT(*)
		FROM messages m
		WHERE m.rowid NOT IN (SELECT message_rowid FROM embedded_messages)
		  AND length(trim(m.content)) >= 20`,
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

	// Newest first. The backlog on a large archive is hours of work, and the
	// messages worth reaching for semantically are the recent ones — embedding
	// in rowid order leaves this week unsearchable until the whole history is
	// done.
	rows, err := rc.DB.Query(`
		SELECT m.rowid, m.content
		FROM messages m
		WHERE m.rowid NOT IN (SELECT message_rowid FROM embedded_messages)
		  AND length(trim(m.content)) >= 20
		ORDER BY m.rowid DESC`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Collect rowid/content pairs first so we can use a write transaction.
	type embedItem struct {
		rowid   int64
		content string
	}
	var items []embedItem
	for rows.Next() {
		var item embedItem
		if err := rows.Scan(&item.rowid, &item.content); err != nil {
			continue
		}
		items = append(items, item)
	}
	rows.Close()

	showProgress := isTTY && !verbose && !rc.JSON
	start := time.Now()
	var done int

	// Process in batches with a transaction per batch.
	const batchSize = 100
	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[i:end]

		tx, err := rc.DB.Begin()
		if err != nil {
			return err
		}

		vecStmt, err := tx.Prepare(
			"INSERT OR IGNORE INTO messages_vec(embedding, message_rowid, chunk_index, chunk_start, chunk_end) VALUES (?, ?, ?, ?, ?)",
		)
		if err != nil {
			tx.Rollback()
			return err
		}
		ownStmt, err := tx.Prepare(
			"INSERT OR REPLACE INTO messages_vec_owner(vec_rowid, message_rowid) VALUES (?, ?)")
		if err != nil {
			vecStmt.Close()
			tx.Rollback()
			return err
		}
		trackStmt, err := tx.Prepare(
			"INSERT OR IGNORE INTO embedded_messages(message_rowid) VALUES (?)")
		if err != nil {
			vecStmt.Close()
			tx.Rollback()
			return err
		}

		for _, item := range batch {
			for idx, ch := range chunkText(item.content) {
				vec, err := embedder.EmbedDocument(ch.text)
				if err != nil {
					continue
				}
				serialized, err := serializeVec(vec)
				if err != nil {
					continue
				}
				res, err := vecStmt.Exec(serialized, item.rowid, idx, ch.start, ch.end)
				if err != nil {
					continue
				}
				// Only claim the rowid when the insert actually happened —
				// LastInsertId on an ignored INSERT reports the previous row.
				if n, err := res.RowsAffected(); err != nil || n == 0 {
					continue
				}
				if vrid, err := res.LastInsertId(); err == nil && vrid != 0 {
					_, _ = ownStmt.Exec(vrid, item.rowid)
				}
			}
			_, _ = trackStmt.Exec(item.rowid)
			done++
			if showProgress {
				printProgress("embedding", done, total, start)
			}
		}

		vecStmt.Close()
		ownStmt.Close()
		trackStmt.Close()
		if err := tx.Commit(); err != nil {
			return err
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

func (cmd *IndexCmd) indexSession(rc *RunContext, roots []string) error {
	var matches []string
	for _, root := range roots {
		patterns := []string{
			filepath.Join(root, "*", cmd.Session+".jsonl"),
			filepath.Join(root, "*", "*_"+cmd.Session+".jsonl"),
		}
		for _, pattern := range patterns {
			m, err := filepath.Glob(pattern)
			if err != nil {
				return err
			}
			matches = append(matches, m...)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("session %s not found", cmd.Session)
	}

	tx, err := rc.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts, err := prepareIndexStmts(tx)
	if err != nil {
		return err
	}
	defer stmts.Close()

	if err := indexFile(tx, stmts, matches[0]); err != nil {
		return err
	}
	return tx.Commit()
}

// sessionFiles lists the session transcripts in one project directory.
// Claude Code names a transcript after its session id. Subagent transcripts
// (agent-*.jsonl, or under subagents/) are stamped with the parent's
// sessionId, so they are not sessions of their own and are skipped.
func sessionFiles(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	var files []string
	for _, path := range matches {
		if isSessionTranscript(path) {
			files = append(files, path)
		}
	}
	return files
}

func isSessionTranscript(path string) bool {
	return !strings.HasPrefix(filepath.Base(path), "agent-")
}

// sessionIDFromPath returns the session UUID encoded in a transcript's file
// name. Claude uses <uuid>.jsonl; pi uses <timestamp>_<uuid>.jsonl. The file
// name is authoritative for Claude forks, whose copied lines retain the parent
// sessionId.
func sessionIDFromPath(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndexByte(stem, '_'); i >= 0 {
		stem = stem[i+1:]
	}
	if len(stem) != 36 || strings.Count(stem, "-") != 4 {
		return ""
	}
	for _, c := range stem {
		if c != '-' && !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return stem
}

type fileState struct {
	mtime float64
	size  int64
}

// loadIndexedFiles loads all indexed file states into a map for fast lookup.
func loadIndexedFiles(db *sql.DB) (map[string]fileState, error) {
	rows, err := db.Query("SELECT path, mtime, size FROM indexed_files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]fileState)
	for rows.Next() {
		var path string
		var fs fileState
		if err := rows.Scan(&path, &fs.mtime, &fs.size); err != nil {
			continue
		}
		m[path] = fs
	}
	return m, nil
}

// needsIndexing checks if a file has changed since last index using the in-memory map.
func needsIndexing(indexed map[string]fileState, path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	stored, ok := indexed[path]
	if !ok {
		return true
	}
	mtime := float64(info.ModTime().UnixMicro()) / 1e6
	return mtime != stored.mtime || info.Size() != stored.size
}

// indexStmts holds pre-prepared statements for batch indexing.
type indexStmts struct {
	existingMsgs   *sql.Stmt
	selectVecRows  *sql.Stmt
	deleteVec      *sql.Stmt
	deleteVecOwner *sql.Stmt
	deleteEmbed    *sql.Stmt
	deleteMsg      *sql.Stmt
	upsertSession  *sql.Stmt
	insertMsg      *sql.Stmt
	insertTool     *sql.Stmt
	insertFile     *sql.Stmt
}

func prepareIndexStmts(tx *sql.Tx) (*indexStmts, error) {
	var s indexStmts
	var err error

	s.existingMsgs, err = tx.Prepare("SELECT id, rowid FROM messages WHERE session_id = ?")
	if err != nil {
		return nil, err
	}
	// message_rowid is an auxiliary column of messages_vec, which sqlite-vec
	// cannot filter on: matching against it full-scans the vector store and
	// reads a blob per row. Resolve the vec rowids from the owner map instead
	// and delete them one at a time, which vec0 treats as a point delete.
	s.selectVecRows, err = tx.Prepare(
		"SELECT vec_rowid FROM messages_vec_owner WHERE message_rowid = ?")
	if err != nil {
		return nil, err
	}
	s.deleteVec, err = tx.Prepare("DELETE FROM messages_vec WHERE rowid = ?")
	if err != nil {
		return nil, err
	}
	s.deleteVecOwner, err = tx.Prepare("DELETE FROM messages_vec_owner WHERE message_rowid = ?")
	if err != nil {
		return nil, err
	}
	s.deleteEmbed, err = tx.Prepare("DELETE FROM embedded_messages WHERE message_rowid = ?")
	if err != nil {
		return nil, err
	}
	s.deleteMsg, err = tx.Prepare("DELETE FROM messages WHERE rowid = ?")
	if err != nil {
		return nil, err
	}
	s.upsertSession, err = tx.Prepare(`
		INSERT INTO sessions (id, project_path, project_name, model, git_branch, started_at, updated_at, source_path, source_mtime, source_size, provenance)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_path = excluded.project_path,
			project_name = excluded.project_name,
			model = excluded.model,
			git_branch = excluded.git_branch,
			started_at = excluded.started_at,
			updated_at = excluded.updated_at,
			source_path = excluded.source_path,
			source_mtime = excluded.source_mtime,
			source_size = excluded.source_size,
			provenance = excluded.provenance`)
	if err != nil {
		return nil, err
	}
	s.insertMsg, err = tx.Prepare(`
		INSERT OR IGNORE INTO messages (id, session_id, parent_id, role, content, timestamp, is_compact_summary, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	s.insertTool, err = tx.Prepare(`
		INSERT OR IGNORE INTO tool_uses (id, message_id, session_id, tool_name, tool_input_summary)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	s.insertFile, err = tx.Prepare(
		"INSERT OR REPLACE INTO indexed_files (path, mtime, size) VALUES (?, ?, ?)")
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *indexStmts) Close() {
	s.existingMsgs.Close()
	s.selectVecRows.Close()
	s.deleteVec.Close()
	s.deleteVecOwner.Close()
	s.deleteEmbed.Close()
	s.deleteMsg.Close()
	s.upsertSession.Close()
	s.insertMsg.Close()
	s.insertTool.Close()
	s.insertFile.Close()
}

// indexFile parses a JSONL file and inserts its contents using pre-prepared statements.
func indexFile(tx *sql.Tx, stmts *indexStmts, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

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

		// Fast-skip lines we don't care about before full JSON parse.
		if !isRelevantLine(line) {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}

		msgType := unquote(raw["type"])

		// Pi stores metadata in a session header and messages under type=message.
		// Claude Code stores metadata alongside type=user/type=assistant entries.
		if msgType == "session" {
			extractPiSessionMeta(&sess, raw)
			continue
		}
		if sess.id == "" {
			extractSessionMeta(&sess, raw)
		}
		// Fill in cwd from later messages if the first one didn't have it.
		if sess.projectPath == "" {
			if cwd := unquote(raw["cwd"]); cwd != "" {
				sess.projectPath, sess.projectName = resolveProject(cwd)
			}
		}
		// Update branch from later messages if present.
		if s := unquote(raw["gitBranch"]); s != "" {
			sess.gitBranch = s
		}

		switch msgType {
		case "user":
			msg := parseUserMessage(raw)
			if msg != nil {
				messages = append(messages, *msg)
				updateSessionTimes(&sess, msg.timestamp)
			}
		case "assistant":
			msg, toolUses := parseAssistantMessage(raw)
			if msg != nil {
				messages = append(messages, *msg)
				tools = append(tools, toolUses...)
				if sess.model == "" {
					sess.model = msg.model
				}
				updateSessionTimes(&sess, msg.timestamp)
			}
		case "message":
			msg, toolUses := parsePiMessage(raw, sess.id)
			if msg != nil {
				messages = append(messages, *msg)
				tools = append(tools, toolUses...)
				if sess.model == "" {
					sess.model = msg.model
				}
				updateSessionTimes(&sess, msg.timestamp)
			}
		}
	}

	mtime := float64(info.ModTime().UnixMicro()) / 1e6

	if id := sessionIDFromPath(path); id != "" {
		sess.id = id
	}
	if sess.id == "" {
		// No session found, but record the file so we skip it next time.
		_, _ = stmts.insertFile.Exec(path, mtime, info.Size())
		return nil
	}

	// Fallback: derive project from the parent directory name in the file path.
	if sess.projectPath == "" {
		dirName := filepath.Base(filepath.Dir(path))
		sess.projectPath = dirName
		sess.projectName = dirName
	}

	// Messages already indexed for this session keep their rows, and with them
	// their rowids and embeddings; transcripts only ever grow, so a re-index
	// is normally an append. Anything indexed earlier that the transcript no
	// longer contains is removed below.
	stale, err := existingMessageRowids(stmts, sess.id)
	if err != nil {
		return fmt.Errorf("reading existing messages: %w", err)
	}

	if sess.provenance == "" {
		sess.provenance = "claude_code"
	}
	_, err = stmts.upsertSession.Exec(
		sess.id, sess.projectPath, sess.projectName, sess.model, sess.gitBranch,
		sess.startedAt, sess.updatedAt, path, mtime, info.Size(), sess.provenance,
	)
	if err != nil {
		return fmt.Errorf("upserting session: %w", err)
	}

	for _, m := range messages {
		delete(stale, m.id)
		_, err = stmts.insertMsg.Exec(
			m.id,
			sess.id,
			m.parentID,
			m.role,
			m.content,
			m.timestamp,
			m.isCompactSummary,
			m.inputTokens,
			m.outputTokens,
		)
		if err != nil {
			return fmt.Errorf("inserting message: %w", err)
		}
	}

	for _, t := range tools {
		_, err = stmts.insertTool.Exec(t.id, t.messageID, sess.id, t.toolName, t.inputSummary)
		if err != nil {
			return fmt.Errorf("inserting tool_use: %w", err)
		}
	}

	for _, rowid := range stale {
		if err := deleteStaleMessage(stmts, rowid); err != nil {
			return err
		}
	}

	// Record file as indexed.
	_, _ = stmts.insertFile.Exec(path, mtime, info.Size())
	return nil
}

// pruneOrphanVectors removes vectors whose message no longer exists, plus
// their owner-map and embedding-tracking rows, and returns how many vectors
// went. Orphans take top-k slots in vector search and are then filtered out,
// so they shrink results without saying so. The owner map makes this a set of
// point deletes rather than a scan of the vector store.
func pruneOrphanVectors(db *sql.DB) (int, error) {
	rows, err := db.Query(`
		SELECT vec_rowid, message_rowid FROM messages_vec_owner o
		WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.rowid = o.message_rowid)`)
	if err != nil {
		return 0, err
	}
	type orphan struct{ vec, msg int64 }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.vec, &o.msg); err != nil {
			rows.Close()
			return 0, err
		}
		orphans = append(orphans, o)
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, o := range orphans {
		if _, err := tx.Exec("DELETE FROM messages_vec WHERE rowid = ?", o.vec); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			"DELETE FROM messages_vec_owner WHERE vec_rowid = ?",
			o.vec,
		); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			"DELETE FROM embedded_messages WHERE message_rowid = ?",
			o.msg,
		); err != nil {
			return 0, err
		}
	}
	// Tracking rows can outlive both their message and its vectors; without
	// this a re-added message with the same rowid would never be embedded.
	if _, err := tx.Exec(`
		DELETE FROM embedded_messages
		WHERE message_rowid NOT IN (SELECT rowid FROM messages)`); err != nil {
		return 0, err
	}
	return len(orphans), tx.Commit()
}

// existingMessageRowids returns id -> rowid for every message already stored
// for the session.
func existingMessageRowids(stmts *indexStmts, sessionID string) (map[string]int64, error) {
	rows, err := stmts.existingMsgs.Query(sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]int64)
	for rows.Next() {
		var id string
		var rowid int64
		if err := rows.Scan(&id, &rowid); err != nil {
			return nil, err
		}
		m[id] = rowid
	}
	return m, rows.Err()
}

// deleteStaleMessage removes one message and everything derived from it: its
// chunk vectors (by vec rowid, via the owner map), its owner-map and
// embedding-tracking rows, and the message row itself, whose delete cascades
// to tool_uses and the FTS index.
func deleteStaleMessage(stmts *indexStmts, rowid int64) error {
	rows, err := stmts.selectVecRows.Query(rowid)
	if err != nil {
		return fmt.Errorf("resolving stale vectors: %w", err)
	}
	var vecRowids []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		vecRowids = append(vecRowids, v)
	}
	rows.Close()
	for _, v := range vecRowids {
		if _, err := stmts.deleteVec.Exec(v); err != nil {
			return fmt.Errorf("deleting stale vector: %w", err)
		}
	}
	if _, err := stmts.deleteVecOwner.Exec(rowid); err != nil {
		return fmt.Errorf("deleting stale vector owner: %w", err)
	}
	if _, err := stmts.deleteEmbed.Exec(rowid); err != nil {
		return fmt.Errorf("deleting stale embedding record: %w", err)
	}
	if _, err := stmts.deleteMsg.Exec(rowid); err != nil {
		return fmt.Errorf("deleting stale message: %w", err)
	}
	return nil
}

// isRelevantLine does a cheap byte scan to check if a JSONL line contains
// a message type we care about, avoiding a full JSON parse for irrelevant lines.
func isRelevantLine(line []byte) bool {
	// Claude: user/assistant entries and any line carrying session metadata.
	// Pi: the session header and nested message entries.
	return bytes.Contains(line, []byte(`"type":"user"`)) ||
		bytes.Contains(line, []byte(`"type":"assistant"`)) ||
		bytes.Contains(line, []byte(`"type":"message"`)) ||
		bytes.Contains(line, []byte(`"type":"session"`)) ||
		bytes.Contains(line, []byte(`"sessionId"`))
}

type sessionMeta struct {
	id          string
	projectPath string
	projectName string
	model       string
	gitBranch   string
	startedAt   string
	updatedAt   string
	provenance  string
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
	sess.provenance = "claude_code"

	cwd := unquote(raw["cwd"])
	if cwd != "" {
		sess.projectPath, sess.projectName = resolveProject(cwd)
	}

	sess.gitBranch = unquote(raw["gitBranch"])
}

func extractPiSessionMeta(sess *sessionMeta, raw map[string]json.RawMessage) {
	id := unquote(raw["id"])
	if id == "" {
		return
	}
	sess.id = id
	sess.provenance = "pi"
	if cwd := unquote(raw["cwd"]); cwd != "" {
		sess.projectPath, sess.projectName = resolveProject(cwd)
	}
	if ts := unquote(raw["timestamp"]); ts != "" {
		sess.startedAt = ts
		sess.updatedAt = ts
	}
}

func updateSessionTimes(sess *sessionMeta, timestamp string) {
	if timestamp == "" {
		return
	}
	if sess.startedAt == "" {
		sess.startedAt = timestamp
	}
	sess.updatedAt = timestamp
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
	textParts, tools := parseContentBlocks(msgObj.Content, msgID, "tool_use", "input")
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

// parsePiMessage converts pi's type=message envelope into the same user and
// assistant rows used for Claude Code transcripts. Tool-result messages remain
// excluded; assistant tool calls are retained as metadata.
func parsePiMessage(
	raw map[string]json.RawMessage,
	sessionID string,
) (*parsedMessage, []parsedToolUse) {
	var msgObj struct {
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content json.RawMessage
		Usage   struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw["message"], &msgObj); err != nil {
		return nil, nil
	}
	if msgObj.Role != "user" && msgObj.Role != "assistant" {
		return nil, nil
	}

	entryID := unquote(raw["id"])
	msgID := piScopedID(sessionID, entryID)
	parentID := piScopedID(sessionID, unquote(raw["parentId"]))
	textParts, tools := parseContentBlocks(msgObj.Content, msgID, "toolCall", "arguments")
	content := strings.Join(textParts, "\n")
	if content == "" && len(tools) == 0 {
		return nil, nil
	}
	for i := range tools {
		tools[i].id = piScopedID(sessionID, tools[i].id)
	}

	return &parsedMessage{
		id:           msgID,
		parentID:     parentID,
		role:         msgObj.Role,
		content:      content,
		timestamp:    unquote(raw["timestamp"]),
		model:        msgObj.Model,
		inputTokens:  msgObj.Usage.Input,
		outputTokens: msgObj.Usage.Output,
	}, tools
}

func piScopedID(sessionID, id string) string {
	if id == "" {
		return ""
	}
	return sessionID + ":" + id
}

// parseContentBlocks extracts searchable text and tool metadata from either
// Claude's snake_case or pi's camelCase assistant content blocks.
func parseContentBlocks(
	contentRaw json.RawMessage,
	messageID, toolBlockType, toolInputField string,
) ([]string, []parsedToolUse) {
	var textParts []string
	var tools []parsedToolUse

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		var s string
		if json.Unmarshal(contentRaw, &s) == nil && s != "" {
			textParts = append(textParts, s)
		}
		return textParts, tools
	}
	for _, block := range blocks {
		switch unquote(block["type"]) {
		case "text":
			if text := unquote(block["text"]); text != "" {
				textParts = append(textParts, text)
			}
		case toolBlockType:
			tu := parsedToolUse{
				id:        unquote(block["id"]),
				messageID: messageID,
				toolName:  unquote(block["name"]),
			}
			tu.inputSummary = extractToolSummary(tu.toolName, block[toolInputField])
			tools = append(tools, tu)
		}
	}
	return textParts, tools
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

	switch strings.ToLower(toolName) {
	case "bash":
		return unquote(input["command"])
	case "read", "write", "edit", "ls":
		if path := unquote(input["path"]); path != "" {
			return path
		}
		return unquote(input["file_path"])
	case "grep", "glob", "find":
		return unquote(input["pattern"])
	case "agent":
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
