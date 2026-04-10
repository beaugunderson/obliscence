package main

import (
	"fmt"
	"strings"
)

type SearchCmd struct {
	Query   string `arg:"" help:"Search query."`
	Project string `help:"Filter by project name." short:"p"`
	Role    string `help:"Filter by role (user, assistant)." short:"r"`
	Limit   int    `help:"Max results." default:"20" short:"l"`
	After   string `help:"Only results after this date (YYYY-MM-DD)." short:"a"`
	Before  string `help:"Only results before this date (YYYY-MM-DD)." short:"b"`
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
			snippet(messages_fts, 0, '[', ']', '...', 32) as snip,
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

	var results []SearchResult

	for rows.Next() {
		var r SearchResult
		err := rows.Scan(
			&r.MessageID,
			&r.SessionID,
			&r.Slug,
			&r.Project,
			&r.Role,
			&r.Timestamp,
			&r.Snippet,
			&r.Score,
			&r.GitBranch,
		)
		if err != nil {
			return err
		}
		results = append(results, r)
	}

	if rc.JSON {
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
		fmt.Printf("  %s\n\n", r.Snippet)
	}

	return nil
}
