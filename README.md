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
obliscence import <export.zip> # Import a claude.ai data export (idempotent)
obliscence search "query"     # Full-text search (BM25)
obliscence search --semantic  # Vector similarity search
obliscence search --hybrid    # FTS5 + semantic via reciprocal rank fusion
obliscence corrections        # Find user messages that correct/push back on the assistant
obliscence sessions           # List recent sessions
obliscence show <session-id>  # Display a conversation (full UUID or unique prefix)
obliscence resume <session-id> # Resume a session in Claude Code
obliscence projects           # List all projects
obliscence stats              # Database statistics
```

### Importing claude.ai chats

Claude Code sessions are indexed automatically. To also search your **claude.ai** web chats, export them (claude.ai → Settings → Privacy → Export data — you're emailed a zip) and import:

```
obliscence import ~/Downloads/data-*.zip
```

Accepts a `.zip`, an extracted export directory, or a `conversations.json`. Idempotent — each conversation/message keys on its claude.ai uuid, so re-importing the same export (or a newer, overlapping one) only adds what's new. Imported chats land under the `claude.ai` project (filter with `search -p claude.ai`).

### Search flags

```
--project, -p   Filter by project name (substring match)
--role, -r      Filter by role: user, assistant
--limit, -l     Max results (default 20)
--after, -a     Results after date (YYYY-MM-DD)
--before, -b    Results before date (YYYY-MM-DD)
--semantic         Vector similarity search (requires setup)
--hybrid           FTS5 + semantic via reciprocal rank fusion
--semantic-weight  Hybrid: weight semantic results vs keyword (default 1.0; >1 favors conceptual matches)
--sort             relevance (default) or recent
--json             Machine-readable output
```

### Examples

```
obliscence search "authentication" --project canvas-plugins --role assistant
obliscence search "how to fix flaky tests" --semantic
obliscence search "database migration" --hybrid
obliscence search "chronius" --sort=recent
obliscence sessions --project hyperscribe --limit 10
obliscence show 8a95221a
obliscence search "terraform" --json | jq '.[].snippet'
```

## Setup

`obliscence setup` does everything:

1. Downloads ONNX Runtime + snowflake-arctic-embed-s model + tokenizer for semantic search
2. Installs Claude Code hooks in `~/.claude/settings.json`:
   - `SessionStart` — runs a full incremental scan (with embeddings) to catch any sessions that ended without firing `SessionEnd` (terminal closed, process killed, etc.)
   - `SessionEnd` — indexes the conversation when a session ends cleanly
   - `PreCompact` — indexes before context compaction so no messages are lost
   - All run `obliscence hook` asynchronously with suppressed output
3. Installs the `/search-history` skill so Claude uses obliscence proactively

All inference runs locally — no API calls, no server process.

To remove everything: `obliscence uninstall` (removes hooks, skill, and downloaded models).

## Semantic search

`--semantic` finds results by meaning — "how to fix flaky tests" matches discussions about test reliability even without the word "flaky". `--hybrid` merges keyword and semantic results via reciprocal rank fusion. Embeddings are generated during `obliscence index` (skip with `--no-embed`).

## Corrections

`obliscence corrections` finds the user messages where you corrected, redirected, or pushed back on the assistant — a speech-act that keyword and semantic search both miss. It's a deterministic lexical scorer (negation/imperative/frustration cues, emphasis, terse phrasing) over already-indexed data, with a length cap and compaction-summary exclusion to keep out injected skill/agent text. Useful for distilling recurring frictions into CLAUDE.md rules.

```
obliscence corrections --min-score 8              # high precision
obliscence corrections --project home-app --after 2026-05-01 --json
```

`--min-score` is the precision dial (≥8 is almost all genuine corrections; lower widens recall and admits some questions/excitement). Other flags: `--max-len`, `--project`, `--after`/`--before`, `--sort score|recent`.

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

Stored at `~/.obliscence/db.sqlite` (override with `--db` or `OBLISCENCE_DB`). FTS5 with Porter stemming for keyword search, sqlite-vec with snowflake-arctic-embed-s (384-dim) for semantic search. Long messages are chunked into overlapping windows so a query matching any part of a message still finds it.

Suggested alias: `alias ob=obliscence`
