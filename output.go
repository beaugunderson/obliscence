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

// printJSON marshals v to stdout as indented JSON.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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
