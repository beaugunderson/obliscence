# obliscence

Archive and search Claude Code conversations. SQLite + FTS5/BM25 + sqlite-vec semantic search.

## Install

```
make install
```

Requires CGo (mattn/go-sqlite3 + daulet/tokenizers). On macOS, Xcode command-line tools are sufficient. The Makefile auto-downloads `libtokenizers.a`.

## Usage

```
obliscence setup              # Download ONNX model for semantic search (~120MB)
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

## Semantic search

`obliscence setup` downloads the ONNX Runtime and sentence-transformers/all-MiniLM-L6-v2 model (~120MB total) to `~/.obliscence/models/`. All inference runs locally — no API calls, no server process.

Embeddings are generated during `obliscence index` (skip with `--no-embed`). `--semantic` finds results by meaning — "how to fix flaky tests" matches discussions about test reliability even without the word "flaky". `--hybrid` merges keyword and semantic results via reciprocal rank fusion.

## Performance

| Operation | Time |
|-----------|------|
| Full index (text only) | ~10s for ~1,800 sessions |
| Full index (with embeddings) | ~5min for ~36k messages |
| Incremental re-index | ~300ms |
| FTS5 search | instant |
| Semantic search | ~1s |
| DB size (text only) | ~28 MB |
| DB size (with embeddings) | ~59 MB |

## Incremental indexing

`obliscence index` scans `~/.claude/projects/` for JSONL files. Only new or changed files are processed (tracked by mtime + size). Re-indexing with no changes takes ~300ms.

## Claude Code hooks

Add to `~/.claude/settings.json` for automatic indexing at session end and before compaction:

```json
{
  "hooks": {
    "SessionEnd": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "obliscence hook",
            "async": true,
            "suppressOutput": true
          }
        ]
      }
    ],
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "obliscence hook",
            "async": true,
            "suppressOutput": true
          }
        ]
      }
    ]
  }
}
```

## Claude Code skill

Copy `.claude/skills/search-history/` to `~/.claude/skills/` for Claude to use this tool proactively. The skill description teaches Claude to invoke obliscence automatically when you ask about past work, previous sessions, or prior solutions — no need to type `/search-history` explicitly.

## Database

Stored at `~/.obliscence/db.sqlite` (override with `--db` or `OBLISCENCE_DB`). FTS5 with Porter stemming for keyword search, sqlite-vec with all-MiniLM-L6-v2 (384-dim) for semantic search.

Suggested alias: `alias ob=obliscence`
