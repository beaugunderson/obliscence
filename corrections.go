package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CorrectionsCmd surfaces user messages that correct, push back on, or express
// frustration with the assistant. This is a speech-act search — finding "the
// user overruled me" rather than a topic — which semantic and keyword search
// both miss. It's the raw material for distilling recurring frictions into
// CLAUDE.md rules.
type CorrectionsCmd struct {
	Project  string `help:"Filter by project name."                                                                                                             short:"p"`
	After    string `help:"Only results after this date (YYYY-MM-DD)."                                                                                          short:"a"`
	Before   string `help:"Only results before this date (YYYY-MM-DD)."                                                                                         short:"b"`
	Limit    int    `help:"Max results."                                                                                                                        short:"l" default:"30"`
	MinScore int    `help:"Minimum correction score."                                                                                                                     default:"3"     name:"min-score"`
	MaxLen   int    `help:"Ignore messages longer than this (chars) — real corrections are terse; long user-role text is injected skill/summary/agent content."           default:"300"   name:"max-len"`
	Sort     string `help:"Sort order: score (default) or recent."                                                                                                        default:"score"                  enum:"score,recent"`
}

// correctionPattern is a weighted cue. Higher weight = stronger signal that the
// message is correcting the assistant.
type correctionPattern struct {
	re     *regexp.Regexp
	weight int
}

// strongCues are explicit corrections / frustration (weight 3).
// mediumCues are directive negations / redirections (weight 2).
// weakCues are softer signals (weight 1).
var (
	strongCues = compileCues(3,
		`\bno+,?\s+(don'?t|do\s*not|stop|please|that)\b`,
		`\bdon'?t\s+do\s+that\b`,
		`\bdidn'?t\s+ask\b`,
		`\bi\s+(already\s+)?(told|said\s+to)\s+you\b`,
		`\bi\s+said\b`,
		`\bwhy\s+(did|are|would)\s+you\b`,
		`\bwho\s+(told|asked)\s+you\b`,
		`\byou\s+keep\b`,
		`\bstop\s+(doing|it|that)\b`,
		`\bthat'?s\s+(not|wrong|incorrect)\b`,
		`\bnot\s+what\s+i\b`,
		`\b(fuck|fucking|shit|goddamn|wtf|christ|jesus)\b`,
		`\b(revert|undo)\b`,
		`\byou\s+(broke|ruined|messed\s+up)\b`,
	)
	mediumCues = compileCues(2,
		`\binstead\b`,
		`\bdon'?t\b`,
		`\bdo\s+not\b`,
		`\bnever\b`,
		`\b(wrong|incorrect)\b`,
		`\bshould(n'?t|\s+not|\s+have)\b`,
		`\bnot\s+what\b`,
		`\bagain\b`,
		`\bplease\s+(don'?t|stop)\b`,
	)
	weakCues = compileCues(1,
		`\bjust\b`,
		`\bactually\b`,
		`\bwait\b`,
		`\bplease\b`,
		`\bisn'?t\b`,
		`\bwasn'?t\b`,
	)

	// leadingCues at the very start of a message are a strong corrective signal.
	leadingCue = regexp.MustCompile(
		`(?i)^\s*(no|nope|stop|wrong|actually|ugh|wait|don'?t|why|that'?s\s+not)\b`,
	)

	// excitementCue marks praise/delight. Profanity, caps, and "!" fire for both
	// frustration ("fix the fucking bug") and enthusiasm ("that's fucking
	// awesome") — a positive marker disambiguates toward the latter, which is
	// almost never a correction.
	excitementCue = regexp.MustCompile(
		`(?i)\b(haha+|lol|lmao|rofl|wow|whoa|woah|woo+|amazing|awesome|incredible|epic|beautiful|gorgeous|brilliant|love (it|this|that)|nice (work|job)|great (work|job)|so (cool|good)|high.quality)\b|🎉|😂|🔥|❤`,
	)
)

func compileCues(weight int, pats ...string) []correctionPattern {
	out := make([]correctionPattern, len(pats))
	for i, p := range pats {
		out[i] = correctionPattern{re: regexp.MustCompile(`(?i)` + p), weight: weight}
	}
	return out
}

// correctionScore rates how strongly a message reads as correcting the
// assistant. It is deterministic and content-only.
func correctionScore(content string) int {
	if strings.TrimSpace(content) == "" {
		return 0
	}
	score := 0
	for _, group := range [][]correctionPattern{strongCues, mediumCues, weakCues} {
		for _, c := range group {
			if c.re.MatchString(content) {
				score += c.weight
			}
		}
	}

	// A corrective opener (e.g. "no, ...", "stop", "why did you") is high-signal.
	if leadingCue.MatchString(content) {
		score += 3
	}

	// Emphasis: exclamation marks and shouted words, each capped.
	score += min(strings.Count(content, "!"), 2)
	score += min(countShoutedWords(content), 2)

	// Terse messages that carry any cue are usually pointed corrections.
	if score > 0 && len(content) < 80 {
		score++
	}

	// Praise/delight disambiguates profanity and emphasis toward enthusiasm, not
	// correction. Genuine corrections rarely contain these.
	if excitementCue.MatchString(content) {
		score -= 5
	}

	if score < 0 {
		score = 0
	}
	return score
}

// countShoutedWords counts ALL-CAPS words of 3+ letters (e.g. "STOP", "NO").
func countShoutedWords(s string) int {
	n := 0
	for _, f := range strings.Fields(s) {
		letters, upper := 0, 0
		for _, r := range f {
			if unicode.IsLetter(r) {
				letters++
				if unicode.IsUpper(r) {
					upper++
				}
			}
		}
		if letters >= 3 && upper == letters {
			n++
		}
	}
	return n
}

func (cmd *CorrectionsCmd) Run(rc *RunContext) error {
	var where []string
	var args []interface{}
	where = append(where, "m.role = 'user'")
	// Compaction/continuation summaries are injected as user-role text and read
	// as corrections (they quote the conversation). Exclude them.
	where = append(where, "m.is_compact_summary = 0")
	if cmd.Project != "" {
		where = append(where, "s.project_name LIKE ?")
		args = append(args, "%"+cmd.Project+"%")
	}
	if cmd.After != "" {
		where = append(where, "m.timestamp >= ?")
		args = append(args, afterBound(cmd.After))
	}
	if cmd.Before != "" {
		where = append(where, "m.timestamp <= ?")
		args = append(args, beforeBound(cmd.Before))
	}

	rows, err := rc.DB.Query(fmt.Sprintf(`
		SELECT m.id, m.session_id, s.project_name, m.role, m.timestamp, m.content, s.git_branch
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s`, strings.Join(where, " AND ")), args...)
	if err != nil {
		return fmt.Errorf("corrections: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var content string
		if err := rows.Scan(
			&r.MessageID, &r.SessionID, &r.Project, &r.Role, &r.Timestamp, &content, &r.GitBranch,
		); err != nil {
			return err
		}
		// Real corrections are terse; long user-role text is injected skill,
		// summary, or agent-prompt content.
		if utf8.RuneCountInString(content) > cmd.MaxLen {
			continue
		}
		s := correctionScore(content)
		if s < cmd.MinScore {
			continue
		}
		r.Score = float64(s)
		r.Snippet = truncate(strings.TrimSpace(content), 200)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if cmd.Sort == "recent" {
		sort.SliceStable(results, func(i, j int) bool {
			return results[i].Timestamp > results[j].Timestamp
		})
	} else {
		// Highest score first; break ties by recency.
		sort.SliceStable(results, func(i, j int) bool {
			if results[i].Score != results[j].Score {
				return results[i].Score > results[j].Score
			}
			return results[i].Timestamp > results[j].Timestamp
		})
	}
	if len(results) > cmd.Limit {
		results = results[:cmd.Limit]
	}

	return cmd.print(rc, results)
}

func (cmd *CorrectionsCmd) print(rc *RunContext, results []SearchResult) error {
	if rc.JSON {
		return printResultsJSON(results)
	}
	if len(results) == 0 {
		fmt.Println("no corrections found")
		return nil
	}
	for _, r := range results {
		ts := r.Timestamp
		if len(ts) >= 10 {
			ts = ts[:10]
		}
		fmt.Printf("%s  %s  %s\n      %s\n",
			yellow(fmt.Sprintf("score %2.0f", r.Score)),
			dim(ts),
			bold(r.Project),
			highlightSnippet(r.Snippet),
		)
	}
	return nil
}
