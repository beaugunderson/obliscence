# obliscence

Archive and search Claude Code conversations. SQLite + FTS5/BM25 + sqlite-vec semantic search.

## Install

```
make install
```

Requires CGo (mattn/go-sqlite3 + daulet/tokenizers). On macOS, Xcode command-line tools are sufficient. The Makefile auto-downloads `libtokenizers.a`.

## Usage

```
obliscence setup              # Download ONNX model for semantic search
obliscence index              # Index new/changed sessions from ~/.claude/projects/
obliscence search "query"     # Full-text search with BM25 ranking
obliscence sessions           # List recent sessions
obliscence show <slug-or-id>  # Display a conversation
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
--semantic      Use vector similarity search (requires setup)
--hybrid        Combine FTS5 + semantic via reciprocal rank fusion
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

`obliscence setup` downloads the ONNX Runtime and sentence-transformers/all-MiniLM-L6-v2 model (~120MB total) to `~/.obliscence/models/`. All inference runs locally — no API calls. Embeddings are generated during `obliscence index` (skip with `--no-embed`).

`--semantic` finds results by meaning, not keywords — "how to fix flaky tests" matches discussions about test reliability even without the word "flaky". `--hybrid` merges keyword and semantic results via reciprocal rank fusion.

## Incremental indexing

On first run, `obliscence index` scans all JSONL files in `~/.claude/projects/`. Subsequent runs only process new or changed files (tracked by mtime + size). A full index of ~1,800 sessions takes ~9s; incremental re-index with no changes takes ~300ms.

## Claude Code hooks

Add to `~/.claude/settings.json` for automatic indexing:

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

Copy `.claude/commands/search-history.md` to your global commands directory for Claude to use this tool via `/search-history`.

## Database

Stored at `~/.obliscence/db.sqlite` (override with `--db` or `OBLISCENCE_DB`).

Uses FTS5 with Porter stemming for keyword search, sqlite-vec with all-MiniLM-L6-v2 embeddings for semantic search.

Suggested alias: `alias ob=obliscence`
