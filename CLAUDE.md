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
- Scope flags (`--project`, `--role`, `--after`, `--before`, `--source`) come from one builder in `filterClauses` and constrain each retrieval branch's candidate set before ranking or fusion. Filtering a branch's results instead would let excluded rows spend its top-k slots, shrinking a scoped search's output without saying so
  - vec0 rejects a WHERE constraint on an auxiliary column, so the KNN query resolves the filters to chunk rowids in a `rowid IN (...)` prefilter, reading the auxiliary columns back through a MATERIALIZED CTE the planner cannot push the join into. Costs ~350ms on ~140k chunks, only when a flag is set
  - sqlite-vec caps KNN `k` at 4096 (`maxVecK`); a `--limit` whose candidate pool exceeds it is an error naming the cap
- A flag a mode cannot act on is rejected in `validate`, never ignored: `--exact` with `--semantic`, `--semantic-weight` without `--hybrid`, `--semantic` with `--hybrid`. A silently dropped scope answers a different question than the one asked, and the results look right
- `--sort=recent` orders all matches by timestamp in keyword mode; in semantic/hybrid it orders the relevance pool, since similarity search has no match/no-match boundary to enumerate
- Asymmetric retrieval: queries get a prefix (`EmbedQuery`), documents do not (`EmbedDocument`)
- Long messages are chunked into overlapping windows; each chunk is a separate vec row, deduped to the nearest chunk per message at search time. Snippet is the matching chunk
- Tokenizer (`tokenizer.json`) is downloaded once at setup and loaded from disk — never per-embed over the network
- `embedSchemaVersion` (PRAGMA user_version) gates a re-embed: bump it when the model/pooling/vec layout changes and the next `index` re-embeds everything
- DynamicAdvancedSession reuses the ONNX session across embed calls
- Incremental indexing via `indexed_files` table tracking mtime + size
- Re-indexing a changed file is an append: the session row is upserted, messages and tool uses are `INSERT OR IGNORE`d by UUID so existing rows keep their rowids and embeddings, and only messages the transcript no longer contains are deleted. Transcripts only grow, so the stale path is rare; `--force` re-parses every file without touching embeddings
  - `messages_vec_owner` maps each vec rowid to its message rowid in an ordinary B-tree, because vec0 answers a filter on its auxiliary `message_rowid` column with a full scan that prepares a SQL statement per row (~700ms on 214k rows). A stale message's vectors are resolved there and deleted by vec rowid, a point operation. Existing DBs backfill the map from sqlite-vec's shadow tables at open (0.6s for 214k rows). vec0 never reuses a deleted slot, so deleting and re-inserting vectors leaves dead space in the table
  - the embed pass runs newest message first, so a large backlog makes recent work searchable before old history
  - every `index` run starts with `pruneOrphanVectors`: vectors, owner-map rows, and `embedded_messages` rows whose message is gone are deleted by rowid. Orphans take top-k slots in vector search and are then filtered out, shrinking results without saying so
  - `tool_uses(message_id)` is indexed because the delete cascade from messages runs one `DELETE ... WHERE message_id = ?` per message; unindexed it full-scans `tool_uses` per message (58ms each on a 215k-row table). A test asserts the cascade's query plan has no `SCAN tool_uses`
- Every `~/.claude*/projects` directory is indexed (`.claude`, `.claude-personal`, `.claude-teams`, ...), auto-discovered by glob so a new profile needs no config. Sessions key on UUID and files on absolute path, so roots coexist without collision. Only `<root>/<project>/*.jsonl` is scanned, and `agent-*.jsonl` (top-level or under `subagents/`) is skipped: subagent transcripts are stamped with the parent's `sessionId`, so indexing one would clobber the parent session
  - the session id comes from the file name when it is a UUID (`sessionIDFromPath`), not from the lines: a forked session's transcript starts with lines copied from its parent that still carry the parent's `sessionId`
- `resume` derives the owning profile from the session's `source_path` (the dir above `projects/`) when launching `claude --resume`. A non-default profile is passed as `CLAUDE_CONFIG_DIR`; the default `~/.claude` instead strips any inherited `CLAUDE_CONFIG_DIR` so Claude Code falls back to `~/.claude` and its `~/.claude.json` machine state (setting `CLAUDE_CONFIG_DIR=~/.claude` would wrongly redirect it to `~/.claude/.claude.json`)
- Project name derived from `cwd` field in JSONL messages (`filepath.Base(cwd)`)
- Hook handler swallows all errors to avoid Claude Code's false "hook error" label bug
- `skillContent` in `setup.go` is the source of truth for the `/search-history` skill: `setup` overwrites the installed copy, so edit the template, never `~/.claude/skills/`. A test asserts every search flag appears there or is listed in `skillOmits` with a reason, so a new flag can't ship without a skill decision
  - each `~/.claude*` profile symlinks `skills` to `~/.claude/skills`, so one write reaches every profile
- User + assistant messages indexed; tool output excluded (too noisy)
- Tool uses stored as metadata only (tool name + summarized input)
- Empty-content messages skipped during embedding and filtered from search results
- `corrections` is a deterministic lexical scorer (no model): weighted negation/imperative/frustration cues + emphasis, gated by a length cap and `is_compact_summary` exclusion to drop injected skill/summary/agent text. Finds a speech-act that keyword/semantic search can't

## Database

`~/.obliscence/db.sqlite` — tables: sessions, messages, tool_uses, messages_fts (FTS5), messages_vec (sqlite-vec), indexed_files.

## Models

`~/.obliscence/models/` — ONNX Runtime shared library + snowflake-arctic-embed-s ONNX model + tokenizer.json. Downloaded by `obliscence setup` (int8-quantized model on arm64, fp32 elsewhere). Optional — FTS5 search works without them.
