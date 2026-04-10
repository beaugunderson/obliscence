package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// SessionsCmd lists sessions.
type SessionsCmd struct {
	Project string `help:"Filter by project name." short:"p"`
	Limit   int    `help:"Max results." default:"20" short:"l"`
}

type SessionRow struct {
	ID          string `json:"id"`
	ProjectName string `json:"project"`
	Slug        string `json:"slug"`
	Model       string `json:"model"`
	GitBranch   string `json:"git_branch"`
	StartedAt   string `json:"started_at"`
	UpdatedAt   string `json:"updated_at"`
	Messages    int    `json:"messages"`
}

func (cmd *SessionsCmd) Run(rc *RunContext) error {
	var where []string
	var args []interface{}

	if cmd.Project != "" {
		where = append(where, "s.project_name LIKE ?")
		args = append(args, "%"+cmd.Project+"%")
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT s.id, s.project_name, s.slug, s.model, s.git_branch, s.started_at, s.updated_at,
			(SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id) as msg_count
		FROM sessions s
		%s
		ORDER BY s.updated_at DESC
		LIMIT ?`, whereClause)
	args = append(args, cmd.Limit)

	rows, err := rc.DB.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	var sessions []SessionRow

	for rows.Next() {
		var s SessionRow
		var model, branch, slug sql.NullString
		err := rows.Scan(&s.ID, &s.ProjectName, &slug, &model, &branch, &s.StartedAt, &s.UpdatedAt, &s.Messages)
		if err != nil {
			return err
		}
		s.Slug = slug.String
		s.Model = model.String
		s.GitBranch = branch.String
		sessions = append(sessions, s)
	}

	if rc.JSON {
		return printJSON(sessions)
	}

	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.StripEscape)
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		tabBold("PROJECT"), tabBold("SLUG"), tabBold("BRANCH"), tabBold("UPDATED"), tabBold("MSGS"))

	for _, s := range sessions {
		updated := s.UpdatedAt
		if len(updated) > 16 {
			updated = updated[:16]
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n",
			s.ProjectName, s.Slug, s.GitBranch, updated, s.Messages)
	}
	return w.Flush()
}

// ShowCmd displays a session conversation.
type ShowCmd struct {
	Session string `arg:"" help:"Session UUID or slug."`
}

func (cmd *ShowCmd) Run(rc *RunContext) error {
	// Try by ID first, then by slug.
	var sessionID string
	err := rc.DB.QueryRow("SELECT id FROM sessions WHERE id = ? OR slug = ?", cmd.Session, cmd.Session).Scan(&sessionID)
	if err == sql.ErrNoRows {
		// Try partial match on slug.
		err = rc.DB.QueryRow("SELECT id FROM sessions WHERE slug LIKE ?", "%"+cmd.Session+"%").Scan(&sessionID)
	}
	if err != nil {
		return fmt.Errorf("session not found: %s", cmd.Session)
	}

	rows, err := rc.DB.Query(`
		SELECT role, content, timestamp, is_compact_summary
		FROM messages
		WHERE session_id = ?
		ORDER BY timestamp ASC`, sessionID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type showMessage struct {
		Role             string `json:"role"`
		Content          string `json:"content"`
		Timestamp        string `json:"timestamp"`
		IsCompactSummary bool   `json:"is_compact_summary"`
	}

	var msgs []showMessage
	for rows.Next() {
		var m showMessage
		if err := rows.Scan(&m.Role, &m.Content, &m.Timestamp, &m.IsCompactSummary); err != nil {
			return err
		}
		msgs = append(msgs, m)
	}

	if rc.JSON {
		return printJSON(msgs)
	}

	for _, m := range msgs {
		label := green("user")
		if m.Role == "assistant" {
			label = yellow("assistant")
		}
		if m.IsCompactSummary {
			label = dim("[compacted]")
		}

		ts := ""
		if len(m.Timestamp) >= 19 {
			ts = dim(m.Timestamp[:19])
		}

		fmt.Printf("%s %s\n%s\n\n", label, ts, m.Content)
	}
	return nil
}

// StatsCmd shows database statistics.
type StatsCmd struct{}

type StatsResult struct {
	Sessions   int    `json:"sessions"`
	Messages   int    `json:"messages"`
	ToolUses   int    `json:"tool_uses"`
	Projects   int    `json:"projects"`
	DBSize     string `json:"db_size"`
	OldestDate string `json:"oldest"`
	NewestDate string `json:"newest"`
}

func (cmd *StatsCmd) Run(rc *RunContext) error {
	var stats StatsResult

	rc.DB.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&stats.Sessions)
	rc.DB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&stats.Messages)
	rc.DB.QueryRow("SELECT COUNT(*) FROM tool_uses").Scan(&stats.ToolUses)
	rc.DB.QueryRow("SELECT COUNT(DISTINCT project_name) FROM sessions").Scan(&stats.Projects)
	rc.DB.QueryRow("SELECT COALESCE(MIN(CASE WHEN started_at != '' THEN started_at END), MIN(updated_at), '') FROM sessions").Scan(&stats.OldestDate)
	rc.DB.QueryRow("SELECT COALESCE(MAX(updated_at), '') FROM sessions").Scan(&stats.NewestDate)

	// DB file size.
	var seq int
	var name, dbPath string
	if err := rc.DB.QueryRow("PRAGMA database_list").Scan(&seq, &name, &dbPath); err == nil && dbPath != "" {
		if fi, err := os.Stat(dbPath); err == nil {
			stats.DBSize = formatBytes(fi.Size())
		}
	}

	if rc.JSON {
		return printJSON(stats)
	}

	fmt.Printf("sessions:  %d\n", stats.Sessions)
	fmt.Printf("messages:  %d\n", stats.Messages)
	fmt.Printf("tool uses: %d\n", stats.ToolUses)
	fmt.Printf("projects:  %d\n", stats.Projects)
	fmt.Printf("db size:   %s\n", stats.DBSize)
	fmt.Printf("oldest:    %s\n", stats.OldestDate)
	fmt.Printf("newest:    %s\n", stats.NewestDate)
	return nil
}

// ProjectsCmd lists all projects.
type ProjectsCmd struct {
	Limit int `help:"Max results." default:"50" short:"l"`
}

type ProjectRow struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Sessions    int    `json:"sessions"`
	LastUpdated string `json:"last_updated"`
}

func (cmd *ProjectsCmd) Run(rc *RunContext) error {
	rows, err := rc.DB.Query(`
		SELECT project_name, project_path, COUNT(*) as session_count, MAX(updated_at) as last_updated
		FROM sessions
		GROUP BY project_name
		ORDER BY last_updated DESC
		LIMIT ?`, cmd.Limit)
	if err != nil {
		return err
	}
	defer rows.Close()

	var projects []ProjectRow
	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.Name, &p.Path, &p.Sessions, &p.LastUpdated); err != nil {
			return err
		}
		projects = append(projects, p)
	}

	if rc.JSON {
		return printJSON(projects)
	}

	if len(projects) == 0 {
		fmt.Println("no projects")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.StripEscape)
	fmt.Fprintf(w, "%s\t%s\t%s\n", tabBold("PROJECT"), tabBold("SESSIONS"), tabBold("LAST UPDATED"))
	for _, p := range projects {
		updated := p.LastUpdated
		if len(updated) > 10 {
			updated = updated[:10]
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", p.Name, p.Sessions, updated)
	}
	return w.Flush()
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
