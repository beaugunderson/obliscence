# obliscence

Go CLI for archiving and searching Claude Code conversations.

## Build

```
make build
```

CGo required (mattn/go-sqlite3, sqlite-vec, daulet/tokenizers). The Makefile auto-downloads `libtokenizers.a` and sets `CGO_LDFLAGS`/`CGO_CFLAGS`.

## Architecture

Flat structure, single package. Each file maps to a concern:

- `main.go` — kong CLI dispatch
- `db.go` — SQLite schema, open, RunContext
- `index.go` — JSONL parsing, incremental indexing, embedding generation
- `import.go` — claude.ai data-export ingestion (zip/dir/conversations.json → sessions/messages, idempotent via uuid keys + upsert); shares the embed pass with `index`
- `search.go` — FTS5 search, semantic search, hybrid (RRF) search
- `corrections.go` — lexical detector for user messages that correct/push back on the assistant
- `sessions.go` — sessions/show/stats/projects/resume commands
- `output.go` — TTY detection, color, JSON output
- `hook.go` — Claude Code hook handler (stdin JSON, always exits 0)
- `embed.go` — ONNX Runtime embedding pipeline (tokenize → infer → CLS pool → L2 normalize), plus message chunking
- `setup.go` — download ONNX Runtime + snowflake-arctic-embed-s model + tokenizer.json
- `resume.go` — resume a session in Claude Code

## Key design decisions

- FTS5 with Porter stemming + BM25 for keyword search; `search -e/--exact` wraps the query tokens in one FTS5 phrase so they must be adjacent and in order (default ANDs the tokens at any distance)
- Grouped (non-JSON) search output is fully chronological: hits within a session, sessions within a project, and projects against each other all sort by timestamp ascending, so the result reads top-to-bottom in the order work happened
- sqlite-vec with snowflake-arctic-embed-s (384-dim, CLS pooling) for semantic search
- Asymmetric retrieval: queries get a prefix (`EmbedQuery`), documents do not (`EmbedDocument`)
- Long messages are chunked into overlapping windows; each chunk is a separate vec row, deduped to the nearest chunk per message at search time. Snippet is the matching chunk
- Tokenizer (`tokenizer.json`) is downloaded once at setup and loaded from disk — never per-embed over the network
- `embedSchemaVersion` (PRAGMA user_version) gates a re-embed: bump it when the model/pooling/vec layout changes and the next `index` re-embeds everything
- DynamicAdvancedSession reuses the ONNX session across embed calls
- Incremental indexing via `indexed_files` table tracking mtime + size
- Project name derived from `cwd` field in JSONL messages (`filepath.Base(cwd)`)
- Hook handler swallows all errors to avoid Claude Code's false "hook error" label bug
- User + assistant messages indexed; tool output excluded (too noisy)
- Tool uses stored as metadata only (tool name + summarized input)
- Empty-content messages skipped during embedding and filtered from search results
- `corrections` is a deterministic lexical scorer (no model): weighted negation/imperative/frustration cues + emphasis, gated by a length cap and `is_compact_summary` exclusion to drop injected skill/summary/agent text. Finds a speech-act that keyword/semantic search can't

## Database

`~/.obliscence/db.sqlite` — tables: sessions, messages, tool_uses, messages_fts (FTS5), messages_vec (sqlite-vec), indexed_files.

## Models

`~/.obliscence/models/` — ONNX Runtime shared library + snowflake-arctic-embed-s ONNX model + tokenizer.json. Downloaded by `obliscence setup` (int8-quantized model on arm64, fp32 elsewhere). Optional — FTS5 search works without them.
