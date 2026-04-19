# remaimber

Archive, search, and manage Claude Code conversations. A Go CLI + MCP server that stores all conversation data in SQLite with FTS5 full-text search.

## Install

### From source (requires Go 1.23+)

```bash
go install github.com/erwint/remaimber/cmd/remaimber@latest
```

### From releases

Download the latest binary from [GitHub Releases](https://github.com/erwint/remaimber/releases).

### Setup

After installing, run setup to configure Claude Code hooks and MCP server:

```bash
remaimber setup
```

This adds:
- **PreCompact hook** — auto-archives before `/compact` destroys data
- **SessionEnd hook** — auto-archives when a session ends
- **MCP server** — lets Claude Code search its own conversation history

## Usage

```bash
# Import all conversations
remaimber import

# Search conversations
remaimber search "sqlite configuration"
remaimber search "auth" --role user --since 2026-01-01

# List sessions
remaimber list
remaimber list --project myproject --json

# Show a session (supports short ID prefixes)
remaimber show abc123

# Export a session
remaimber export --last 1 --format markdown
remaimber export <session-id> --format json

# Move/copy conversation to another project
remaimber move <session-id> <target-project> --copy

# Database management
remaimber stats
remaimber verify
remaimber delete <session-id>

# Shell completions
remaimber completion zsh > "${fpath[1]}/_remaimber"
```

## MCP Tools

When running as an MCP server (`remaimber mcp`), four tools are available:

| Tool | Description |
|------|-------------|
| `search_conversations` | FTS5 search with project/role/date filters |
| `get_session` | Retrieve messages from a specific session |
| `list_sessions` | List sessions with optional filters |
| `move_conversation` | Move or copy a conversation between projects |

## How it works

Claude Code stores conversations as JSONL files in `~/.claude/projects/`. These files get deleted after `cleanupPeriodDays` and are destroyed by `/compact`.

remaimber archives everything into `~/.claude/remaimber/remaimber.db` with:
- **Full conversation memory** — stores all JSONL line types, not filtered
- **FTS5 search** — porter stemming, date/role/project filtering
- **UUID + content-hash dedup** — safe concurrent imports, no duplicates
- **Byte-offset tracking** — incremental imports skip already-processed content
- **Content cleaning** — strips system-injected XML tags from search index
