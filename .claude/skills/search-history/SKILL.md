---
name: search-history
description: Search previous Claude Code conversations for past work, decisions, and solutions. Use when the user asks "what did we do", "remember when", "in a previous session", "how did we solve this before", "last time we worked on", "did we ever", or needs context from past conversations. Also use proactively when the user is about to re-solve a problem that may have been addressed before.
---

Search past Claude Code conversations using obliscence. Use `--json` for structured output, `--semantic` for meaning-based search, `--hybrid` for best results combining keyword + semantic.

## Choosing a search mode

- **Keyword search** (default): Best when the user remembers specific terms, error messages, file names, or tool names.
- **Semantic search** (`--semantic`): Best for conceptual queries like "how did we handle rate limiting" or "what was the approach to testing plugins". Finds results by meaning, not exact words.
- **Hybrid search** (`--hybrid`): Best general-purpose choice when unsure. Combines keyword and semantic via reciprocal rank fusion.

## Commands

```bash
# Search (start with --hybrid for best results)
obliscence search "$QUERY" --hybrid --json

# Keyword-only search (when user mentions specific terms)
obliscence search "$QUERY" --json

# Semantic search (when query is conceptual/natural language)
obliscence search "$QUERY" --semantic --json

# Filter by project
obliscence search "$QUERY" --hybrid --project PROJECT_NAME --json

# Filter by role (user messages vs assistant responses)
obliscence search "$QUERY" --hybrid --role user --json
obliscence search "$QUERY" --hybrid --role assistant --json

# Filter by date range
obliscence search "$QUERY" --hybrid --after 2026-01-01 --before 2026-04-01 --json

# List recent sessions for a project
obliscence sessions --project PROJECT_NAME --json --limit 20

# Show a full conversation
obliscence show SESSION_ID_OR_SLUG

# Resume a past session
obliscence resume SESSION_ID_OR_SLUG

# List all projects
obliscence projects --json

# Database stats
obliscence stats --json
```

## Tips

- Start broad, then narrow with `--project` or `--role` filters.
- Use `obliscence show SLUG` to read the full conversation once you find a relevant result.
- Session slugs (like `warm-wondering-quill`) are stable identifiers — use them to reference sessions.
- When the user says "we worked on X recently", add `--after` with a date ~2 weeks back.
- When results span many projects, add `--project` to focus on the relevant one.
