package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ftsQuery converts free-text into a safe FTS5 MATCH expression. It extracts
// word tokens (neutralizing FTS5 operators and punctuation so a stray "OR"/"-"
// can't turn into a parse error) and joins them. When exact is false each token
// is quoted separately, giving implicit AND, so "belt and suspenders" and
// "belt-and-suspenders" search identically. When exact is true the tokens are
// wrapped in a single quoted phrase, so they must appear adjacent and in order.
// Returns "" when the query contains no word characters.
func ftsQuery(raw string, exact bool) string {
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	if exact {
		return `"` + strings.Join(tokens, " ") + `"`
	}
	for i, t := range tokens {
		tokens[i] = `"` + t + `"`
	}
	return strings.Join(tokens, " ")
}

// maxVecK is sqlite-vec's ceiling on k in a KNN query.
const maxVecK = 4096

type SearchCmd struct {
	Query          string  `arg:"" help:"Search query."`
	Project        string  `       help:"Filter by project name."                                                                      short:"p"`
	Source         string  `       help:"Filter by provenance: 'claude.ai' or 'local' (claude_code)."                                            name:"source"`
	Role           string  `       help:"Filter by role (user, assistant)."                                                            short:"r"`
	Limit          int     `       help:"Max results."                                                                                 short:"l"                        default:"20"`
	After          string  `       help:"Only results after this date (YYYY-MM-DD)."                                                   short:"a"`
	Before         string  `       help:"Only results before this date (YYYY-MM-DD)."                                                  short:"b"`
	Exact          bool    `       help:"Match the query as an exact phrase (adjacent words, in order)."                               short:"e" name:"exact"`
	Semantic       bool    `       help:"Use semantic (vector) search."                                                                          name:"semantic"`
	Hybrid         bool    `       help:"Combine FTS5 and semantic search."                                                                      name:"hybrid"`
	Sort           string  `       help:"Sort order: relevance (default) or recent."                                                                                    default:"relevance" enum:"relevance,recent"`
	SemanticWeight float64 `       help:"Hybrid: weight semantic results relative to keyword (>1 favors semantic/conceptual matches)."           name:"semantic-weight" default:"1.0"`

	prov string // resolved provenance filter, validated in Run
}

type SearchResult struct {
	SessionID string  `json:"session_id"`
	Project   string  `json:"project"`
	Role      string  `json:"role"`
	Timestamp string  `json:"timestamp"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
	GitBranch string  `json:"git_branch,omitempty"`
	MessageID string  `json:"message_id"`
}

func (cmd *SearchCmd) Run(rc *RunContext) error {
	if err := cmd.validate(); err != nil {
		return err
	}
	prov, err := normalizeProvenance(cmd.Source)
	if err != nil {
		return err
	}
	cmd.prov = prov

	var embedder *Embedder
	if cmd.Semantic || cmd.Hybrid {
		mode := "semantic"
		if cmd.Hybrid {
			mode = "hybrid"
		}
		if embedder, err = NewEmbedder(); err != nil {
			return fmt.Errorf("initializing embedder: %w", err)
		}
		if embedder == nil {
			return fmt.Errorf("%s search requires setup — run 'obliscence setup' first", mode)
		}
		defer embedder.Close()
	}

	results, err := cmd.results(rc, embedder)
	if err != nil {
		return err
	}
	return cmd.printResults(rc, results)
}

// validate rejects flag combinations a mode cannot honor. A filter or modifier
// that a mode ignores is an error rather than a no-op, because a silently
// dropped scope yields confident answers over the wrong candidate set.
func (cmd *SearchCmd) validate() error {
	if cmd.Semantic && cmd.Hybrid {
		return fmt.Errorf("--semantic and --hybrid are mutually exclusive; pick one")
	}
	if cmd.Exact && cmd.Semantic {
		return fmt.Errorf(
			"--exact is a keyword-matching mode and has no meaning for --semantic; " +
				"drop --exact, or use --hybrid to phrase-match the keyword half",
		)
	}
	if cmd.SemanticWeight != 1.0 && !cmd.Hybrid {
		return fmt.Errorf(
			"--semantic-weight only applies to --hybrid, which fuses the two rankings",
		)
	}
	if cmd.SemanticWeight <= 0 {
		return fmt.Errorf("--semantic-weight must be greater than 0, got %g", cmd.SemanticWeight)
	}
	return nil
}

// results retrieves the ranked hits for the requested mode. embedder is required
// by the modes that embed the query and ignored by keyword search.
func (cmd *SearchCmd) results(rc *RunContext, embedder *Embedder) ([]SearchResult, error) {
	switch {
	case cmd.Hybrid:
		return cmd.hybridResults(rc, embedder)
	case cmd.Semantic:
		return cmd.semanticResults(rc, embedder)
	default:
		return cmd.ftsResults(rc, cmd.Limit)
	}
}

// semanticResults performs vector similarity search.
func (cmd *SearchCmd) semanticResults(
	rc *RunContext,
	embedder *Embedder,
) ([]SearchResult, error) {
	queryVec, err := embedder.EmbedQuery(cmd.Query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	pool := cmd.Limit * 3
	if cmd.Sort == "recent" {
		pool = cmd.Limit * 10
	}
	results, err := cmd.vectorResults(rc, queryVec, pool)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}

	if cmd.Sort == "recent" {
		sort.Slice(results, func(i, j int) bool {
			return results[i].Timestamp > results[j].Timestamp
		})
	}
	if len(results) > cmd.Limit {
		results = results[:cmd.Limit]
	}
	return results, nil
}

// filterClauses returns the SQL predicates (and their args) for every scope flag,
// written against the aliases m (messages) and s (sessions). Each retrieval
// branch applies these to its own candidate set, so keyword and vector search
// always draw from the same rows.
func (cmd *SearchCmd) filterClauses() ([]string, []interface{}) {
	var where []string
	var args []interface{}

	if cmd.Project != "" {
		where = append(where, "s.project_name LIKE ?")
		args = append(args, "%"+cmd.Project+"%")
	}
	if cmd.Role != "" {
		where = append(where, "m.role = ?")
		args = append(args, cmd.Role)
	}
	if cmd.After != "" {
		where = append(where, "m.timestamp >= ?")
		args = append(args, afterBound(cmd.After))
	}
	if cmd.Before != "" {
		where = append(where, "m.timestamp <= ?")
		args = append(args, beforeBound(cmd.Before))
	}
	if cmd.prov != "" {
		where = append(where, "s.provenance = ?")
		args = append(args, cmd.prov)
	}
	return where, args
}

// normalizeProvenance maps a user-supplied --source value onto a stored
// provenance value ("claude_code" / "claude_ai"). Empty input means no filter.
// An unrecognized value is an error that names the valid options, so a calling
// agent gets actionable feedback.
func normalizeProvenance(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "claude_ai", "claude.ai", "claudeai", "ai", "web":
		return "claude_ai", nil
	case "claude_code", "claudecode", "code", "local", "cc":
		return "claude_code", nil
	default:
		return "", fmt.Errorf(
			"invalid --source %q; valid values: %q (aliases: claude.ai, ai, web) or %q (aliases: local, code, cc)",
			s,
			"claude_ai",
			"claude_code",
		)
	}
}

// hybridResults merges FTS5 and semantic results via reciprocal rank fusion.
func (cmd *SearchCmd) hybridResults(
	rc *RunContext,
	embedder *Embedder,
) ([]SearchResult, error) {
	queryVec, err := embedder.EmbedQuery(cmd.Query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	// Both branches over-fetch by the same factor so fusion has depth to work
	// with on either side.
	branchLimit := cmd.Limit * 2
	ftsResults, err := cmd.ftsResults(rc, branchLimit)
	if err != nil {
		return nil, fmt.Errorf("FTS search: %w", err)
	}
	semResults, err := cmd.vectorResults(rc, queryVec, branchLimit)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}

	// Reciprocal Rank Fusion (k=60). SemanticWeight scales the semantic side so
	// callers can favor conceptual matches over exact-keyword ones.
	const k = 60.0
	semWeight := cmd.SemanticWeight
	scores := make(map[string]float64)
	resultMap := make(map[string]SearchResult)

	for rank, r := range ftsResults {
		scores[r.MessageID] += 1.0 / (k + float64(rank+1))
		resultMap[r.MessageID] = r
	}
	for rank, r := range semResults {
		scores[r.MessageID] += semWeight / (k + float64(rank+1))
		if _, exists := resultMap[r.MessageID]; !exists {
			resultMap[r.MessageID] = r
		}
	}

	// Sort by RRF score descending.
	type scored struct {
		id    string
		score float64
	}
	var ranked []scored
	for id, s := range scores {
		ranked = append(ranked, scored{id, s})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	// Collect top results. When sorting by recency, take a larger pool from
	// the RRF ranking, then re-sort by timestamp before trimming.
	take := cmd.Limit
	if cmd.Sort == "recent" {
		take = cmd.Limit * 5
	}
	var results []SearchResult
	for i, s := range ranked {
		if i >= take {
			break
		}
		r := resultMap[s.id]
		r.Score = s.score
		results = append(results, r)
	}
	if cmd.Sort == "recent" {
		sort.Slice(results, func(i, j int) bool {
			return results[i].Timestamp > results[j].Timestamp
		})
		if len(results) > cmd.Limit {
			results = results[:cmd.Limit]
		}
	}

	return results, nil
}

// ftsResults returns up to limit FTS5/BM25 hits.
func (cmd *SearchCmd) ftsResults(rc *RunContext, limit int) ([]SearchResult, error) {
	match := ftsQuery(cmd.Query, cmd.Exact)
	if match == "" {
		return nil, nil
	}

	where := []string{"messages_fts MATCH ?"}
	args := []interface{}{match}

	filters, filterArgs := cmd.filterClauses()
	where = append(where, filters...)
	args = append(args, filterArgs...)

	orderBy := "score"
	if cmd.Sort == "recent" {
		orderBy = "m.timestamp DESC"
	}
	query := fmt.Sprintf(`
		SELECT
			m.id, m.session_id, s.project_name, m.role, m.timestamp,
			snippet(messages_fts, 0, char(2), char(3), '...', 32) as snip,
			bm25(messages_fts) as score, s.git_branch
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		ORDER BY %s
		LIMIT ?`,
		strings.Join(where, " AND "),
		orderBy,
	)
	args = append(args, limit)

	rows, err := rc.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanResults(rows)
}

// vectorResults returns up to limit nearest-neighbor hits.
func (cmd *SearchCmd) vectorResults(
	rc *RunContext,
	queryVec []float32,
	limit int,
) ([]SearchResult, error) {
	serialized, err := serializeVec(queryVec)
	if err != nil {
		return nil, err
	}

	// Messages are chunked, so multiple vec rows can belong to one message. Fetch
	// the nearest chunks first, dedup to the best chunk per message, then look up
	// message/session data in a second query. Over-fetch chunks so dedup still
	// yields `limit` distinct messages.
	const chunkOverfetch = 4
	k := limit * chunkOverfetch
	if k > maxVecK {
		return nil, fmt.Errorf(
			"--limit %d needs %d vector candidates, more than the %d sqlite-vec allows; use a smaller --limit",
			cmd.Limit,
			k,
			maxVecK,
		)
	}

	where := "embedding MATCH ? AND k = ?"
	args := []interface{}{serialized, k}

	// Scope flags constrain the candidate set, not the results: post-filtering a
	// KNN result lets excluded rows consume the k slots, so a scoped search
	// silently returns fewer hits than exist. vec0 rejects a WHERE constraint on
	// an auxiliary column (message_rowid), but accepts a `rowid IN (...)`
	// prefilter, so resolve the filters to chunk rowids in a subquery. Reading
	// the auxiliary columns back is a scan of them alone (no vectors); the CTE is
	// MATERIALIZED so the planner cannot push the join back down onto them.
	if filters, filterArgs := cmd.filterClauses(); len(filters) > 0 {
		where += fmt.Sprintf(` AND rowid IN (
			WITH chunks AS MATERIALIZED (
				SELECT rowid AS vec_rowid, message_rowid FROM messages_vec
			)
			SELECT c.vec_rowid FROM chunks c
			JOIN messages m ON m.rowid = c.message_rowid
			JOIN sessions s ON s.id = m.session_id
			WHERE %s)`, strings.Join(filters, " AND "))
		args = append(args, filterArgs...)
	}

	rows, err := rc.DB.Query(fmt.Sprintf(
		`SELECT message_rowid, chunk_start, chunk_end, distance
		 FROM messages_vec
		 WHERE %s
		 ORDER BY distance`, where), args...)
	if err != nil {
		return nil, err
	}

	type chunkHit struct {
		rowid      int64
		start, end int
		distance   float64
	}
	var hits []chunkHit
	seen := make(map[int64]bool)
	for rows.Next() {
		var h chunkHit
		if err := rows.Scan(&h.rowid, &h.start, &h.end, &h.distance); err != nil {
			rows.Close()
			return nil, err
		}
		if seen[h.rowid] {
			continue // keep only the nearest chunk per message
		}
		seen[h.rowid] = true
		hits = append(hits, h)
		if len(hits) >= limit {
			break
		}
	}
	rows.Close()
	if len(hits) == 0 {
		return nil, nil
	}

	// Look up message/session data for the matched messages in one query. The
	// candidates are already scoped, so this is a plain lookup.
	placeholders := make([]string, len(hits))
	idArgs := make([]interface{}, len(hits))
	for i, h := range hits {
		placeholders[i] = "?"
		idArgs[i] = h.rowid
	}
	dataRows, err := rc.DB.Query(fmt.Sprintf(`
		SELECT m.rowid, m.id, m.session_id, s.project_name, m.role, m.timestamp, m.content, s.git_branch
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE m.rowid IN (%s)`, strings.Join(placeholders, ",")), idArgs...)
	if err != nil {
		return nil, err
	}
	defer dataRows.Close()

	type msgData struct {
		id, sessionID, project, role, timestamp, content, gitBranch string
	}
	byRowid := make(map[int64]msgData, len(hits))
	for dataRows.Next() {
		var rowid int64
		var d msgData
		if err := dataRows.Scan(
			&rowid,
			&d.id,
			&d.sessionID,
			&d.project,
			&d.role,
			&d.timestamp,
			&d.content,
			&d.gitBranch,
		); err != nil {
			return nil, err
		}
		byRowid[rowid] = d
	}

	// Assemble results in nearest-first order, snippeting the matching chunk.
	var results []SearchResult
	for _, h := range hits {
		d, ok := byRowid[h.rowid]
		if !ok {
			continue
		}
		snippet := truncate(strings.TrimSpace(runeSlice(d.content, h.start, h.end)), 200)
		if snippet == "" {
			continue
		}
		results = append(results, SearchResult{
			MessageID: d.id,
			SessionID: d.sessionID,
			Project:   d.project,
			Role:      d.role,
			Timestamp: d.timestamp,
			Snippet:   snippet,
			Score:     h.distance,
			GitBranch: d.gitBranch,
		})
	}
	return results, nil
}

// runeSlice returns the substring between character offsets [start, end),
// clamped to valid bounds. Offsets are rune positions, matching how chunks were
// recorded during indexing.
func runeSlice(s string, start, end int) string {
	r := []rune(s)
	if start < 0 {
		start = 0
	}
	if end > len(r) {
		end = len(r)
	}
	if start >= end {
		return ""
	}
	return string(r[start:end])
}

func scanResults(rows *sql.Rows) ([]SearchResult, error) {
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		err := rows.Scan(
			&r.MessageID, &r.SessionID, &r.Project,
			&r.Role, &r.Timestamp, &r.Snippet, &r.Score, &r.GitBranch,
		)
		if err != nil {
			return nil, err
		}
		r.Snippet = truncate(strings.TrimSpace(r.Snippet), 200)
		if r.Snippet == "" {
			continue // Skip messages with no text content.
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (cmd *SearchCmd) printResults(rc *RunContext, results []SearchResult) error {
	if rc.JSON {
		for i := range results {
			results[i].Snippet = strings.NewReplacer("\x02", "", "\x03", "").
				Replace(results[i].Snippet)
		}
		return printResultsJSON(results)
	}

	if len(results) == 0 {
		fmt.Println("no results")
		return nil
	}

	// Group by project, then by session UUID, preserving the original
	// (score-sorted) order at every level via first-appearance.
	type sessionGroup struct {
		id   string
		hits []SearchResult
	}
	type projectGroup struct {
		name     string
		sessions []*sessionGroup
		index    map[string]*sessionGroup
	}
	var projects []*projectGroup
	projectIdx := make(map[string]*projectGroup)
	for _, r := range results {
		pg, ok := projectIdx[r.Project]
		if !ok {
			pg = &projectGroup{name: r.Project, index: map[string]*sessionGroup{}}
			projectIdx[r.Project] = pg
			projects = append(projects, pg)
		}
		sg, ok := pg.index[r.SessionID]
		if !ok {
			sg = &sessionGroup{id: r.SessionID}
			pg.index[r.SessionID] = sg
			pg.sessions = append(pg.sessions, sg)
		}
		sg.hits = append(sg.hits, r)
	}

	// Oldest-first at every grouping level so the whole result reads
	// chronologically top-to-bottom: hits within a session by timestamp
	// ascending, sessions within a project by their earliest hit, and projects
	// against each other by their earliest hit.
	earliest := func(sg *sessionGroup) string {
		return sg.hits[0].Timestamp // valid: hits sorted ascending in the loop below
	}
	for _, pg := range projects {
		for _, sg := range pg.sessions {
			sort.Slice(sg.hits, func(i, j int) bool {
				return sg.hits[i].Timestamp < sg.hits[j].Timestamp
			})
		}
		sort.SliceStable(pg.sessions, func(i, j int) bool {
			return earliest(pg.sessions[i]) < earliest(pg.sessions[j])
		})
	}
	projectEarliest := func(pg *projectGroup) string {
		ts := ""
		for _, sg := range pg.sessions {
			if first := earliest(sg); ts == "" || first < ts {
				ts = first
			}
		}
		return ts
	}
	sort.SliceStable(projects, func(i, j int) bool {
		return projectEarliest(projects[i]) < projectEarliest(projects[j])
	})

	for i, pg := range projects {
		if i > 0 {
			fmt.Println()
		}
		fmt.Println(bold(pg.name))
		for j, sg := range pg.sessions {
			if j > 0 {
				fmt.Println()
			}
			fmt.Printf("  %s\n", dim(sg.id))
			for _, r := range sg.hits {
				ts := r.Timestamp
				if len(ts) >= 19 {
					ts = ts[:10] + " " + ts[11:19]
				}
				fmt.Printf("    %s %s  %s\n",
					cyan(fmt.Sprintf("%-9s", r.Role)),
					dim(ts),
					highlightSnippet(r.Snippet),
				)
			}
		}
	}

	return nil
}
