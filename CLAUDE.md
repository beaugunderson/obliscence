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
- `search.go` — FTS5 search, semantic search, hybrid (RRF) search
- `sessions.go` — sessions/show/stats/projects/resume commands
- `output.go` — TTY detection, color, JSON output
- `hook.go` — Claude Code hook handler (stdin JSON, always exits 0)
- `embed.go` — ONNX Runtime embedding pipeline (tokenize → infer → mean pool → L2 normalize)
- `setup.go` — download ONNX Runtime + all-MiniLM-L6-v2 model
- `resume.go` — resume a session in Claude Code

## Key design decisions

- FTS5 with Porter stemming + BM25 for keyword search
- sqlite-vec with all-MiniLM-L6-v2 (384-dim) for semantic search
- DynamicAdvancedSession reuses the ONNX session across embed calls
- Incremental indexing via `indexed_files` table tracking mtime + size
- Project name derived from `cwd` field in JSONL messages (`filepath.Base(cwd)`)
- Hook handler swallows all errors to avoid Claude Code's false "hook error" label bug
- User + assistant messages indexed; tool output excluded (too noisy)
- Tool uses stored as metadata only (tool name + summarized input)
- Empty-content messages skipped during embedding and filtered from search results

## Database

`~/.obliscence/db.sqlite` — tables: sessions, messages, tool_uses, messages_fts (FTS5), messages_vec (sqlite-vec), indexed_files.

## Models

`~/.obliscence/models/` — ONNX Runtime shared library + all-MiniLM-L6-v2 ONNX model. Downloaded by `obliscence setup`. Optional — FTS5 search works without them.
