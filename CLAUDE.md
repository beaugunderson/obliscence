# obliscence

Go CLI for archiving and searching Claude Code conversations.

## Build

```
go build -tags "sqlite_fts5" .
```

CGo is required (mattn/go-sqlite3 + sqlite-vec).

## Architecture

Flat structure, single package. Each file maps to a concern:

- `main.go` — kong CLI dispatch
- `db.go` — SQLite schema, open, RunContext
- `index.go` — JSONL parsing, incremental indexing
- `search.go` — FTS5 search with BM25
- `sessions.go` — sessions/show/stats/projects commands
- `output.go` — TTY detection, color, JSON output
- `hook.go` — Claude Code hook handler (stdin JSON, always exits 0)

## Key design decisions

- FTS5 with Porter stemming for search (not vector search yet)
- Incremental indexing via `indexed_files` table tracking mtime + size
- Project name derived from `cwd` field in JSONL messages (`filepath.Base(cwd)`)
- Hook handler swallows all errors to avoid Claude Code's false "hook error" label bug
- User + assistant messages indexed; tool output excluded (too noisy)
- Tool uses stored as metadata only (tool name + summarized input)

## Database

`~/.obliscence/db.sqlite` — tables: sessions, messages, tool_uses, messages_fts (FTS5), messages_vec (sqlite-vec, empty for now), indexed_files.
