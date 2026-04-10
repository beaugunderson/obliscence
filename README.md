# obliscence

Archive and search Claude Code conversations. SQLite + FTS5 + BM25.

## Install

```
go install -tags "sqlite_fts5" github.com/beaugunderson/obliscence@latest
```

Requires CGo (for mattn/go-sqlite3). On macOS, Xcode command-line tools are sufficient.

## Usage

```
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
--json          Machine-readable output
```

### Examples

```
obliscence search "authentication" --project canvas-plugins --role assistant
obliscence sessions --project hyperscribe --limit 10
obliscence show warm-wondering-quill
obliscence search "terraform" --json | jq '.[].snippet'
```

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

Uses FTS5 with Porter stemming for search. sqlite-vec is wired up for future vector search.

Suggested alias: `alias ob=obliscence`
