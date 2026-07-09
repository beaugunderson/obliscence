package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ANSI color codes.
const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

var isTTY bool

func init() {
	fi, err := os.Stdout.Stat()
	if err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}
}

func color(code, s string) string {
	if !isTTY {
		return s
	}
	return code + s + colorReset
}

func bold(s string) string   { return color(colorBold, s) }
func dim(s string) string    { return color(colorDim, s) }
func green(s string) string  { return color(colorGreen, s) }
func yellow(s string) string { return color(colorYellow, s) }
func cyan(s string) string   { return color(colorCyan, s) }

// tabBold wraps ANSI codes in \xff delimiters so tabwriter.StripEscape
// excludes them from column width calculations.
func tabBold(s string) string {
	if !isTTY {
		return s
	}
	return "\xff" + colorBold + "\xff" + s + "\xff" + colorReset + "\xff"
}

// highlightSnippet replaces FTS5 match markers (STX/ETX control chars) with
// green ANSI coloring for TTY output, or strips them for non-TTY.
func highlightSnippet(s string) string {
	if isTTY {
		s = strings.ReplaceAll(s, "\x02", colorGreen+colorBold)
		s = strings.ReplaceAll(s, "\x03", colorReset)
	} else {
		s = strings.ReplaceAll(s, "\x02", "")
		s = strings.ReplaceAll(s, "\x03", "")
	}
	return s
}

// printJSON marshals v to stdout as indented JSON.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printResults emits a result slice as JSON, encoding an empty result set as
// `[]` rather than `null` so downstream consumers can always parse an array.
func printResults(results []SearchResult) error {
	if results == nil {
		results = []SearchResult{}
	}
	return printJSON(results)
}

// afterBound and beforeBound normalize a bare YYYY-MM-DD date filter into a
// timestamp comparison that includes the entire named day. Timestamps are
// stored as full ISO-8601 strings (e.g. 2026-06-13T07:31:55.941Z), so a naive
// `timestamp <= '2026-06-13'` excludes all of June 13. A full timestamp (or any
// value already carrying a time component) is returned unchanged.
func afterBound(s string) string {
	if isBareDate(s) {
		return s + "T00:00:00.000Z"
	}
	return s
}

func beforeBound(s string) string {
	if isBareDate(s) {
		return s + "T23:59:59.999Z"
	}
	return s
}

func isBareDate(s string) bool {
	return len(s) == 10 && s[4] == '-' && s[7] == '-'
}

// truncate shortens s to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// snippet extracts a snippet around the first occurrence of query in text.
func snippet(text, query string, contextChars int) string {
	lower := strings.ToLower(text)
	q := strings.ToLower(query)

	// Try each word in the query.
	words := strings.Fields(q)
	idx := -1
	matchWord := q
	for _, w := range words {
		idx = strings.Index(lower, w)
		if idx >= 0 {
			matchWord = w
			break
		}
	}
	if idx < 0 {
		return truncate(text, contextChars*2)
	}

	start := idx - contextChars
	if start < 0 {
		start = 0
	}
	end := idx + len(matchWord) + contextChars
	if end > len(text) {
		end = len(text)
	}

	s := text[start:end]
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")

	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "..."
	}
	if end < len(text) {
		suffix = "..."
	}

	return fmt.Sprintf("%s%s%s", prefix, s, suffix)
}
