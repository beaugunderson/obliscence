package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

type SearchCmd struct {
	Query    string `arg:"" help:"Search query."`
	Project  string `help:"Filter by project name." short:"p"`
	Role     string `help:"Filter by role (user, assistant)." short:"r"`
	Limit    int    `help:"Max results." default:"20" short:"l"`
	After    string `help:"Only results after this date (YYYY-MM-DD)." short:"a"`
	Before   string `help:"Only results before this date (YYYY-MM-DD)." short:"b"`
	Semantic bool   `help:"Use semantic (vector) search." name:"semantic"`
	Hybrid   bool   `help:"Combine FTS5 and semantic search." name:"hybrid"`
}

type SearchResult struct {
	SessionID string  `json:"session_id"`
	Slug      string  `json:"slug"`
	Project   string  `json:"project"`
	Role      string  `json:"role"`
	Timestamp string  `json:"timestamp"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
	GitBranch string  `json:"git_branch,omitempty"`
	MessageID string  `json:"message_id"`
}

func (cmd *SearchCmd) Run(rc *RunContext) error {
	if cmd.Hybrid {
		return cmd.runHybrid(rc)
	}
	if cmd.Semantic {
		return cmd.runSemantic(rc)
	}
	return cmd.runFTS(rc)
}

// runFTS performs a standard FTS5/BM25 search.
func (cmd *SearchCmd) runFTS(rc *RunContext) error {
	var where []string
	var args []interface{}

	where = append(where, "messages_fts MATCH ?")
	args = append(args, cmd.Query)

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
		args = append(args, cmd.After)
	}
	if cmd.Before != "" {
		where = append(where, "m.timestamp <= ?")
		args = append(args, cmd.Before)
	}

	query := fmt.Sprintf(`
		SELECT
			m.id,
			m.session_id,
			s.slug,
			s.project_name,
			m.role,
			m.timestamp,
			snippet(messages_fts, 0, char(2), char(3), '...', 32) as snip,
			bm25(messages_fts) as score,
			s.git_branch
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		ORDER BY score
		LIMIT ?`,
		strings.Join(where, " AND "),
	)
	args = append(args, cmd.Limit)

	rows, err := rc.DB.Query(query, args...)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	return cmd.collectAndPrint(rc, rows, false)
}

// runSemantic performs vector similarity search.
func (cmd *SearchCmd) runSemantic(rc *RunContext) error {
	embedder, err := NewEmbedder()
	if err != nil {
		return fmt.Errorf("initializing embedder: %w", err)
	}
	if embedder == nil {
		return fmt.Errorf("semantic search requires setup — run 'obliscence setup' first")
	}
	defer embedder.Close()

	queryVec, err := embedder.Embed(cmd.Query)
	if err != nil {
		return fmt.Errorf("embedding query: %w", err)
	}

	results, err := cmd.semanticResults(rc, queryVec, cmd.Limit*3)
	if err != nil {
		return fmt.Errorf("semantic search: %w", err)
	}

	// Post-filter results.
	results = cmd.filterResults(results)
	if len(results) > cmd.Limit {
		results = results[:cmd.Limit]
	}

	return cmd.printResults(rc, results)
}

// filterResults applies project/role/date filters to results.
func (cmd *SearchCmd) filterResults(results []SearchResult) []SearchResult {
	if cmd.Project == "" && cmd.Role == "" && cmd.After == "" && cmd.Before == "" {
		return results
	}

	var filtered []SearchResult
	for _, r := range results {
		if cmd.Project != "" && !strings.Contains(strings.ToLower(r.Project), strings.ToLower(cmd.Project)) {
			continue
		}
		if cmd.Role != "" && r.Role != cmd.Role {
			continue
		}
		if cmd.After != "" && r.Timestamp < cmd.After {
			continue
		}
		if cmd.Before != "" && r.Timestamp > cmd.Before {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// runHybrid merges FTS5 and semantic results via reciprocal rank fusion.
func (cmd *SearchCmd) runHybrid(rc *RunContext) error {
	embedder, err := NewEmbedder()
	if err != nil {
		return fmt.Errorf("initializing embedder: %w", err)
	}
	if embedder == nil {
		return fmt.Errorf("hybrid search requires setup — run 'obliscence setup' first")
	}
	defer embedder.Close()

	// Get FTS results.
	ftsLimit := cmd.Limit * 2
	origLimit := cmd.Limit
	cmd.Limit = ftsLimit
	ftsResults, err := cmd.ftsResults(rc)
	cmd.Limit = origLimit
	if err != nil {
		return fmt.Errorf("FTS search: %w", err)
	}

	// Get semantic results.
	queryVec, err := embedder.Embed(cmd.Query)
	if err != nil {
		return fmt.Errorf("embedding query: %w", err)
	}
	semResults, err := cmd.semanticResults(rc, queryVec, ftsLimit)
	if err != nil {
		return fmt.Errorf("semantic search: %w", err)
	}

	// Reciprocal Rank Fusion (k=60).
	const k = 60.0
	scores := make(map[string]float64)
	resultMap := make(map[string]SearchResult)

	for rank, r := range ftsResults {
		scores[r.MessageID] += 1.0 / (k + float64(rank+1))
		resultMap[r.MessageID] = r
	}
	for rank, r := range semResults {
		scores[r.MessageID] += 1.0 / (k + float64(rank+1))
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

	// Collect top results.
	var results []SearchResult
	for i, s := range ranked {
		if i >= cmd.Limit {
			break
		}
		r := resultMap[s.id]
		r.Score = s.score
		results = append(results, r)
	}

	return cmd.printResults(rc, results)
}

// ftsResults returns FTS search results as a slice (for hybrid merging).
func (cmd *SearchCmd) ftsResults(rc *RunContext) ([]SearchResult, error) {
	var where []string
	var args []interface{}

	where = append(where, "messages_fts MATCH ?")
	args = append(args, cmd.Query)

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
		args = append(args, cmd.After)
	}
	if cmd.Before != "" {
		where = append(where, "m.timestamp <= ?")
		args = append(args, cmd.Before)
	}

	query := fmt.Sprintf(`
		SELECT
			m.id, m.session_id, s.slug, s.project_name, m.role, m.timestamp,
			snippet(messages_fts, 0, char(2), char(3), '...', 32) as snip,
			bm25(messages_fts) as score, s.git_branch
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		ORDER BY score
		LIMIT ?`,
		strings.Join(where, " AND "),
	)
	args = append(args, cmd.Limit)

	rows, err := rc.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanResults(rows)
}

// semanticResults returns vector search results as a slice (for hybrid merging).
func (cmd *SearchCmd) semanticResults(rc *RunContext, queryVec []float32, limit int) ([]SearchResult, error) {
	serialized, err := serializeVec(queryVec)
	if err != nil {
		return nil, err
	}

	// sqlite-vec KNN queries can only have MATCH + k constraints.
	// Join with messages/sessions after the KNN lookup.
	rows, err := rc.DB.Query(`
		SELECT
			m.id, m.session_id, s.slug, s.project_name, m.role, m.timestamp,
			substr(m.content, 1, 200) as snip,
			knn.distance as score, s.git_branch
		FROM (
			SELECT rowid, distance
			FROM messages_vec
			WHERE embedding MATCH ? AND k = ?
		) knn
		JOIN messages m ON m.rowid = knn.rowid
		JOIN sessions s ON s.id = m.session_id
		ORDER BY knn.distance`,
		serialized, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanResults(rows)
}

func scanResults(rows *sql.Rows) ([]SearchResult, error) {
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		err := rows.Scan(
			&r.MessageID, &r.SessionID, &r.Slug, &r.Project,
			&r.Role, &r.Timestamp, &r.Snippet, &r.Score, &r.GitBranch,
		)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, nil
}

func (cmd *SearchCmd) collectAndPrint(rc *RunContext, rows *sql.Rows, isSemantic bool) error {
	results, err := scanResults(rows)
	if err != nil {
		return err
	}
	return cmd.printResults(rc, results)
}

func (cmd *SearchCmd) printResults(rc *RunContext, results []SearchResult) error {
	if rc.JSON {
		for i := range results {
			results[i].Snippet = strings.NewReplacer("\x02", "", "\x03", "").Replace(results[i].Snippet)
		}
		return printJSON(results)
	}

	if len(results) == 0 {
		fmt.Println("no results")
		return nil
	}

	for _, r := range results {
		fmt.Printf("%s %s %s %s\n",
			bold(r.Project),
			dim(r.Slug),
			cyan(r.Role),
			dim(r.Timestamp[:min(len(r.Timestamp), 10)]),
		)
		fmt.Printf("  %s\n\n", highlightSnippet(r.Snippet))
	}

	return nil
}
