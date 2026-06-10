package main

import (
	"math"
	"strings"
	"testing"
)

func TestChunkText(t *testing.T) {
	// Empty input yields no chunks.
	if got := chunkText(""); got != nil {
		t.Errorf("chunkText(\"\") = %v, want nil", got)
	}

	// Short input (<= chunkSize) is a single chunk spanning the whole string.
	short := "the quick brown fox"
	chunks := chunkText(short)
	if len(chunks) != 1 {
		t.Fatalf("short input: got %d chunks, want 1", len(chunks))
	}
	if chunks[0].text != short || chunks[0].start != 0 || chunks[0].end != len([]rune(short)) {
		t.Errorf("short chunk = %+v, want full span", chunks[0])
	}

	// Long input splits into multiple overlapping chunks.
	long := strings.Repeat("word ", 1000) // 5000 chars
	runes := []rune(long)
	chunks = chunkText(long)
	if len(chunks) < 2 {
		t.Fatalf("long input: got %d chunks, want >= 2", len(chunks))
	}
	for i, ch := range chunks {
		// Offsets must reconstruct the chunk text exactly (substr semantics).
		if want := string(runes[ch.start:ch.end]); ch.text != want {
			t.Errorf("chunk %d: text does not match offsets", i)
		}
		if ch.end-ch.start > chunkSize {
			t.Errorf("chunk %d: length %d exceeds chunkSize %d", i, ch.end-ch.start, chunkSize)
		}
		if i > 0 {
			// Consecutive chunks overlap (start of next < end of prev).
			if ch.start >= chunks[i-1].end {
				t.Errorf("chunk %d does not overlap previous (start %d >= prev end %d)",
					i, ch.start, chunks[i-1].end)
			}
		}
	}
	// The last chunk reaches the end of the string.
	if last := chunks[len(chunks)-1]; last.end != len(runes) {
		t.Errorf("last chunk end = %d, want %d", last.end, len(runes))
	}
}

func TestEmbedderQueryVsDocument(t *testing.T) {
	e := requireEmbedder(t)

	qv, err := e.EmbedQuery("how do I delete a file safely")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(qv) != embeddingDim {
		t.Fatalf("query vec dim = %d, want %d", len(qv), embeddingDim)
	}
	if norm := l2norm(qv); math.Abs(float64(norm)-1.0) > 1e-3 {
		t.Errorf("query vec not unit-normalized: norm = %f", norm)
	}

	dv, err := e.EmbedDocument("how do I delete a file safely")
	if err != nil {
		t.Fatalf("EmbedDocument: %v", err)
	}
	// The asymmetric query prefix must change the embedding.
	if cosine(qv, dv) > 0.999 {
		t.Errorf("query and document embeddings are nearly identical; prefix not applied")
	}

	// Semantic sanity: a relevant document should outrank an irrelevant one.
	relevant, _ := e.EmbedDocument("use trash instead of rm so deleted files can be restored")
	irrelevant, _ := e.EmbedDocument("the weather forecast predicts sunshine all weekend")
	if cosine(qv, relevant) <= cosine(qv, irrelevant) {
		t.Errorf("relevant doc (%.3f) did not outrank irrelevant doc (%.3f)",
			cosine(qv, relevant), cosine(qv, irrelevant))
	}
}

func l2norm(v []float32) float32 {
	var s float32
	for _, x := range v {
		s += x * x
	}
	return float32(math.Sqrt(float64(s)))
}

func cosine(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot // inputs are L2-normalized
}
