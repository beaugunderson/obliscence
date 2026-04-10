# obliscence

Archive and search Claude Code conversations. SQLite + FTS5/BM25 + sqlite-vec semantic search.

## Install

### Homebrew (macOS)

```
brew install beaugunderson/tap/obliscence
obliscence setup
```

### From source

```
git clone https://github.com/beaugunderson/obliscence.git
cd obliscence
make install
obliscence setup
```

Requires CGo (mattn/go-sqlite3 + daulet/tokenizers). The Makefile auto-downloads `libtokenizers.a`.

## Usage

```
obliscence setup              # Download models, install hooks + skill
obliscence uninstall          # Remove hooks, skill, and downloaded models
obliscence index              # Index new/changed sessions
obliscence search "query"     # Full-text search (BM25)
obliscence search --semantic  # Vector similarity search
obliscence search --hybrid    # FTS5 + semantic via reciprocal rank fusion
obliscence sessions           # List recent sessions
obliscence show <slug-or-id>  # Display a conversation
obliscence resume <slug>      # Resume a session in Claude Code
obliscence projects           # List all projects
obliscence stats              # Database statistics
```

### Search flags

```
--project, -p   Filter by project name (substring match)
--role, -r      Filter by role: user, assistant
--limit, -l     Max results (default 20)
--after, -a     Results after date (YYYY-MM-DD)
--before, -b    Results before date (YYYY-MM-DD)
--semantic      Vector similarity search (requires setup)
--hybrid        FTS5 + semantic via reciprocal rank fusion
--json          Machine-readable output
```

### Examples

```
obliscence search "authentication" --project canvas-plugins --role assistant
obliscence search "how to fix flaky tests" --semantic
obliscence search "database migration" --hybrid
obliscence sessions --project hyperscribe --limit 10
obliscence show warm-wondering-quill
obliscence search "terraform" --json | jq '.[].snippet'
```

## Setup

`obliscence setup` does everything:

1. Downloads ONNX Runtime + all-MiniLM-L6-v2 model (~55MB total) for semantic search
2. Installs Claude Code hooks in `~/.claude/settings.json`:
   - `SessionEnd` — indexes the conversation when a session ends
   - `PreCompact` — indexes before context compaction so no messages are lost
   - Both run `obliscence hook` asynchronously with suppressed output
3. Installs the `/search-history` skill so Claude uses obliscence proactively

All inference runs locally — no API calls, no server process.

To remove everything: `obliscence uninstall` (removes hooks, skill, and downloaded models).

## Semantic search

`--semantic` finds results by meaning — "how to fix flaky tests" matches discussions about test reliability even without the word "flaky". `--hybrid` merges keyword and semantic results via reciprocal rank fusion. Embeddings are generated during `obliscence index` (skip with `--no-embed`).

## Performance

| Operation | Time |
|-----------|------|
| Full index (text only) | ~8s for ~1,800 sessions |
| Full index (with embeddings) | ~1min for ~14k messages |
| Incremental re-index | ~2s |
| FTS5 search | instant |
| Semantic search | ~1s |

## Incremental indexing

`obliscence index` scans `~/.claude/projects/` for JSONL files. Only new or changed files are processed (tracked by mtime + size).

## Database

Stored at `~/.obliscence/db.sqlite` (override with `--db` or `OBLISCENCE_DB`). FTS5 with Porter stemming for keyword search, sqlite-vec with all-MiniLM-L6-v2 (384-dim) for semantic search.

Suggested alias: `alias ob=obliscence`
