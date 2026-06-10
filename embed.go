package main

import (
	"fmt"
	"math"
	"os"
	"unicode"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// serializeVec converts a float32 slice to sqlite-vec binary format.
func serializeVec(v []float32) ([]byte, error) {
	return sqlite_vec.SerializeFloat32(v)
}

const maxSeqLen = 512

// Chunking parameters. Long messages are split into overlapping windows so
// each is embedded independently — this avoids diluting one vector across a
// whole message and prevents the tail of a long message from being dropped.
// chunkSize is in characters and stays comfortably under maxSeqLen tokens for
// English (~4 chars/token).
const (
	chunkSize    = 1200
	chunkOverlap = 200
)

// textChunk is a slice of a message, with character offsets (0-based, end
// exclusive) so the matching chunk can be rendered as a snippet via substr.
type textChunk struct {
	text  string
	start int
	end   int
}

// Embedder generates text embeddings using a local ONNX model.
// Create with NewEmbedder; nil means embedding is not available.
type Embedder struct {
	tokenizer *tokenizers.Tokenizer
	session   *ort.DynamicAdvancedSession
}

// NewEmbedder creates an embedder if the model files are available.
// Returns (nil, nil) if setup hasn't been run — this is not an error.
func NewEmbedder() (*Embedder, error) {
	libPath := onnxRuntimeLibPath()
	modelPath := onnxModelPath()
	tokPath := tokenizerPath()

	// Check if model files exist.
	if _, err := os.Stat(libPath); err != nil {
		return nil, nil
	}
	if _, err := os.Stat(modelPath); err != nil {
		return nil, nil
	}
	if _, err := os.Stat(tokPath); err != nil {
		return nil, nil
	}

	// Initialize ONNX Runtime.
	ort.SetSharedLibraryPath(libPath)
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("initializing ONNX Runtime: %w", err)
	}

	// Load tokenizer from the local file downloaded at setup.
	tk, err := tokenizers.FromFile(tokPath)
	if err != nil {
		// InitializeEnvironment succeeded above; release it so a later
		// NewEmbedder can re-initialize the global ONNX environment.
		ort.DestroyEnvironment()
		return nil, fmt.Errorf("loading tokenizer: %w", err)
	}

	// Create a reusable dynamic session — tensors are provided at Run() time.
	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"},
		nil,
	)
	if err != nil {
		tk.Close()
		ort.DestroyEnvironment()
		return nil, fmt.Errorf("creating ONNX session: %w", err)
	}

	return &Embedder{
		tokenizer: tk,
		session:   session,
	}, nil
}

// Close releases resources.
func (e *Embedder) Close() {
	if e == nil {
		return
	}
	if e.session != nil {
		e.session.Destroy()
	}
	if e.tokenizer != nil {
		e.tokenizer.Close()
	}
	ort.DestroyEnvironment()
}

// EmbedQuery embeds a search query. arctic-embed is asymmetric: queries get a
// prefix, documents do not.
func (e *Embedder) EmbedQuery(text string) ([]float32, error) {
	return e.embed(queryPrefix + text)
}

// EmbedDocument embeds a document (a message or message chunk), no prefix.
func (e *Embedder) EmbedDocument(text string) ([]float32, error) {
	return e.embed(text)
}

// embed generates a 384-dimensional embedding for the given text.
func (e *Embedder) embed(text string) ([]float32, error) {
	if e == nil {
		return nil, fmt.Errorf("embedder not available — run 'obliscence setup' first")
	}

	// Truncate very long text to avoid excessive memory use.
	if len(text) > maxSeqLen*10 {
		text = text[:maxSeqLen*10]
	}

	// Tokenize.
	encoding := e.tokenizer.EncodeWithOptions(text, true,
		tokenizers.WithReturnAttentionMask(),
		tokenizers.WithReturnTypeIDs(),
	)

	inputIDs := encoding.IDs
	attentionMask := encoding.AttentionMask
	typeIDs := encoding.TypeIDs

	// Truncate to max sequence length.
	seqLen := len(inputIDs)
	if seqLen > maxSeqLen {
		seqLen = maxSeqLen
		inputIDs = inputIDs[:seqLen]
		attentionMask = attentionMask[:seqLen]
		typeIDs = typeIDs[:seqLen]
	}

	if seqLen == 0 {
		return make([]float32, embeddingDim), nil
	}

	// Create input tensors (new each call — different seq lengths).
	shape := ort.NewShape(1, int64(seqLen))

	inputTensor, err := ort.NewTensor(shape, uint32ToInt64(inputIDs))
	if err != nil {
		return nil, fmt.Errorf("creating input_ids tensor: %w", err)
	}
	defer inputTensor.Destroy()

	attTensor, err := ort.NewTensor(shape, uint32ToInt64(attentionMask))
	if err != nil {
		return nil, fmt.Errorf("creating attention_mask tensor: %w", err)
	}
	defer attTensor.Destroy()

	typeTensor, err := ort.NewTensor(shape, uint32ToInt64(typeIDs))
	if err != nil {
		return nil, fmt.Errorf("creating token_type_ids tensor: %w", err)
	}
	defer typeTensor.Destroy()

	// Create output tensor.
	outputTensor, err := ort.NewEmptyTensor[float32](
		ort.NewShape(1, int64(seqLen), int64(embeddingDim)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating output tensor: %w", err)
	}
	defer outputTensor.Destroy()

	// Run inference on the reused session.
	inputs := []ort.Value{inputTensor, attTensor, typeTensor}
	outputs := []ort.Value{outputTensor}
	if err := e.session.Run(inputs, outputs); err != nil {
		return nil, fmt.Errorf("running inference: %w", err)
	}

	// Pool + L2 normalize. arctic-embed uses CLS pooling (first token);
	// MiniLM-style models use mean pooling.
	tokenEmbeddings := outputTensor.GetData()
	var pooled []float32
	if usesCLSPooling {
		pooled = clsPool(tokenEmbeddings, embeddingDim)
	} else {
		pooled = meanPool(tokenEmbeddings, uint32ToFloat32(attentionMask), seqLen, embeddingDim)
	}
	return l2Normalize(pooled), nil
}

// chunkText splits s into overlapping character windows, preferring to break at
// whitespace near each window boundary. Offsets are character (rune) positions,
// matching SQLite's substr semantics on UTF-8 text.
func chunkText(s string) []textChunk {
	runes := []rune(s)
	n := len(runes)
	if n == 0 {
		return nil
	}
	if n <= chunkSize {
		return []textChunk{{text: s, start: 0, end: n}}
	}

	var chunks []textChunk
	start := 0
	for start < n {
		end := start + chunkSize
		if end >= n {
			end = n
		} else {
			// Snap the boundary back to the last whitespace within a window so
			// chunks don't split mid-word.
			for i := end; i > end-150 && i > start; i-- {
				if unicode.IsSpace(runes[i]) {
					end = i
					break
				}
			}
		}
		chunks = append(chunks, textChunk{
			text:  string(runes[start:end]),
			start: start,
			end:   end,
		})
		if end >= n {
			break
		}
		start = end - chunkOverlap
		if start < 0 {
			start = 0
		}
	}
	return chunks
}

// clsPool returns the first token's embedding (the [CLS] token).
func clsPool(tokenEmbeddings []float32, dim int) []float32 {
	out := make([]float32, dim)
	copy(out, tokenEmbeddings[:dim])
	return out
}

func uint32ToInt64(ids []uint32) []int64 {
	result := make([]int64, len(ids))
	for i, id := range ids {
		result[i] = int64(id)
	}
	return result
}

func uint32ToFloat32(ids []uint32) []float32 {
	result := make([]float32, len(ids))
	for i, id := range ids {
		result[i] = float32(id)
	}
	return result
}

// meanPool computes mean pooling over token embeddings, weighted by attention mask.
func meanPool(tokenEmbeddings, attentionMask []float32, seqLen, dim int) []float32 {
	pooled := make([]float32, dim)
	var maskSum float32

	for i := 0; i < seqLen; i++ {
		if attentionMask[i] > 0 {
			maskSum += attentionMask[i]
			for d := 0; d < dim; d++ {
				pooled[d] += tokenEmbeddings[i*dim+d] * attentionMask[i]
			}
		}
	}

	if maskSum > 0 {
		for d := 0; d < dim; d++ {
			pooled[d] /= maskSum
		}
	}
	return pooled
}

// l2Normalize normalizes a vector to unit length.
func l2Normalize(v []float32) []float32 {
	var sumSq float32
	for _, x := range v {
		sumSq += x * x
	}
	norm := float32(math.Sqrt(float64(sumSq)))

	out := make([]float32, len(v))
	if norm > 0 {
		for i, x := range v {
			out[i] = x / norm
		}
	}
	return out
}
