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
remaimber search "recipe import" --repo .   # only this repo, across all worktrees

# List sessions
remaimber list
remaimber list --project myproject --json
remaimber list --repo . --subpath .         # this repo + current monorepo subpath

# Show a session (supports short ID prefixes)
remaimber show abc123

# Export a session
remaimber export --last 1 --format markdown
remaimber export <session-id> --format json

# Find & resume a session in the CURRENT worktree (cross-worktree)
remaimber resume                            # list this repo's sessions, any worktree
remaimber resume <session-id>               # link it here + print resume options

# Move/copy conversation to another project
remaimber move <session-id> <target-project> --copy

# Rolling summaries (LLM-backed; see Configuration)
remaimber summarize                         # summarize sessions with new activity
remaimber summarize <session-id> --force    # rebuild one session's summary

# Database management
remaimber stats
remaimber verify
remaimber delete <session-id>
remaimber backfill-identity                 # one-time: populate repo identity for old sessions

# Shell completions
remaimber completion zsh > "${fpath[1]}/_remaimber"
```

## MCP Tools

When running as an MCP server (`remaimber mcp`), these tools are available:

| Tool | Description |
|------|-------------|
| `search_conversations` | FTS5 search with project/repo/role/date filters |
| `get_session` | Retrieve messages from a specific session |
| `list_sessions` | List sessions with optional filters (incl. `repo`/`subpath`) |
| `move_conversation` | Move or copy a conversation between projects |
| `link_session` | Link a session into the current project so it can be resumed here |

`search_conversations` and `list_sessions` accept `repo: "."` and `subpath: "."` to mean "the current repo / subpath", resolved from the server's working directory.

## Cross-worktree find & resume

Claude Code keys session storage by launch directory, so the same repo scatters across many project keys (one per worktree) and native `--resume` can't find sessions from other worktrees — or from Agent worktrees that were later deleted.

remaimber captures a **durable identity** for every session at start (a `SessionStart` hook records `repo_id` = `realpath(git --git-common-dir)` and `subpath` = `git rev-parse --show-prefix`). Because it's captured at start, it survives deletion of the worktree. You can then:

- `remaimber list --repo .` — every session for this repo, across all worktrees
- `remaimber resume <id>` — copy the session's transcript under the current directory's project key so `claude --resume <id>` works *here*, no worktree switching

If a chosen session looks like it's still running in another worktree, resume warns you (resuming a live transcript would corrupt it). Run `remaimber backfill-identity` once after upgrading to populate identity for sessions whose worktree still exists.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `REMAIMBER_DB` | `~/.claude/remaimber/remaimber.db` | Database path |
| `REMAIMBER_LLM` | `claude` | Summary backend: `claude` (uses the local CLI) or an OpenAI-compatible base URL (e.g. `http://localhost:11434/v1` for Ollama, `http://localhost:1234/v1` for LM Studio) |
| `REMAIMBER_LLM_MODEL` | `haiku` (claude backend) | Model name for summarization |
| `REMAIMBER_LLM_KEY` | — | Optional bearer token for the HTTP backend |

### Summaries and hooks

Summaries are produced by a **throttled background sweep** (`summarize-if-stale`) wired into several hooks — `SessionStart`, `Notification`, and `SessionEnd`. It deliberately does **not** rely on `SessionEnd` alone, because that event isn't guaranteed to fire (e.g. a corporate VM killed overnight never cleanly ends its sessions). Running on `SessionStart` and `Notification` means a session left unsummarized by an unclean shutdown gets caught up the next time Claude runs. The sweep throttles itself (default 15 min) so firing it often is cheap.

The rolling summary is **offset-based and incremental**, so the sweep also checkpoints *active* sessions (not just finished ones) — each pass folds only the messages added since the last summary. That way, if the machine is killed mid-session, the latest checkpoint (at most one throttle interval old) survives on disk and the session is still recallable by its summary, not just by full-text search. (This assumes `~/.claude` is persisted across restarts, which it normally is — that's where both the archive DB and Claude's transcripts live.)

Both backends run from hooks, including inside a live Claude session:

- **Local/HTTP backend** (Ollama, LM Studio, …): a plain HTTP call, no constraints.
- **`claude` backend**: invoked as `claude -p --no-session-persistence --model haiku`. `--no-session-persistence` means the summarization call creates no session of its own, so it runs fine nested inside a Claude session and fires no lifecycle hooks (no recursion). It needs the `claude` binary and auth available in the hook's environment; where that isn't the case (headless/corporate), use the local/HTTP backend.

Summarization treats the conversation transcript as **untrusted data** — the system prompt instructs the model never to follow instructions found inside it and to reply with only the summary, guarding against prompt injection from archived content.

Liveness does not depend on a clean `SessionEnd`: a session is considered "still running" only if its transcript file was modified in the last few minutes, so a killed session correctly ages out on its own.

## How it works

Claude Code stores conversations as JSONL files in `~/.claude/projects/`. These files get deleted after `cleanupPeriodDays` and are destroyed by `/compact`.

remaimber archives everything into `~/.claude/remaimber/remaimber.db` with:
- **Full conversation memory** — stores all JSONL line types, not filtered
- **FTS5 search** — porter stemming, date/role/project filtering
- **UUID + content-hash dedup** — safe concurrent imports, no duplicates
- **Byte-offset tracking** — incremental imports skip already-processed content
- **Content cleaning** — strips system-injected XML tags from search index
