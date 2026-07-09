package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"
)

func TestDateBounds(t *testing.T) {
	ts := "2026-06-13T07:31:55.941Z" // a real timestamp on 2026-06-13

	// --before 2026-06-13 must INCLUDE timestamps on that day.
	if b := beforeBound("2026-06-13"); !(ts <= b) {
		t.Errorf("beforeBound(2026-06-13)=%q excludes same-day ts %q", b, ts)
	}
	// --before 2026-06-12 must EXCLUDE the 13th.
	if b := beforeBound("2026-06-12"); ts <= b {
		t.Errorf("beforeBound(2026-06-12)=%q wrongly includes %q", b, ts)
	}
	// --after 2026-06-13 must INCLUDE same-day timestamps.
	if a := afterBound("2026-06-13"); !(ts >= a) {
		t.Errorf("afterBound(2026-06-13)=%q excludes same-day ts %q", a, ts)
	}
	// A full timestamp passes through untouched.
	if got := beforeBound(ts); got != ts {
		t.Errorf("beforeBound(full ts) mutated %q -> %q", ts, got)
	}
}

func TestPrintResultsEmptyIsArray(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	var results []SearchResult // nil
	err := printResults(results)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	io.Copy(&buf, r)
	var parsed []any
	if e := json.Unmarshal(buf.Bytes(), &parsed); e != nil {
		t.Fatalf("empty result did not parse as JSON array: %v (got %q)", e, buf.String())
	}
}
