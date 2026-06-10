package main

import (
	"os"
	"testing"
)

// testEmbedder is a single shared embedder for the whole test binary. The ONNX
// runtime environment is process-global and cannot be initialized twice, so all
// embedder-dependent tests reuse this one. It is nil when the model is not set
// up, in which case those tests skip.
var testEmbedder *Embedder

func TestMain(m *testing.M) {
	e, err := NewEmbedder()
	if err == nil {
		testEmbedder = e
	}
	code := m.Run()
	if testEmbedder != nil {
		testEmbedder.Close()
	}
	os.Exit(code)
}

// requireEmbedder returns the shared embedder or skips the test.
func requireEmbedder(t *testing.T) *Embedder {
	t.Helper()
	if testEmbedder == nil {
		t.Skip("embedding model not set up; run 'obliscence setup'")
	}
	return testEmbedder
}
