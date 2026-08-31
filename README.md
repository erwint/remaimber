# remaimber

Archive, search, and manage coding-agent conversations — Claude Code, Codex and pi in one archive. A Go CLI + MCP server that stores all conversation data in SQLite with FTS5 full-text search.

Every agent's sessions land in the same database, keyed to the same projects, so a conversation you had in one is findable and resumable from another.

## Install

### From source (requires Go 1.23+)

```bash
go install github.com/erwint/remaimber/cmd/remaimber@latest
```

### From releases

Download the latest binary from [GitHub Releases](https://github.com/erwint/remaimber/releases).

### Requirements

| | version |
|---|---|
| Codex | **0.148.0 or newer** — asynchronous command hooks; below that the plugin's maintenance hooks are skipped |
| Claude Code | any version with plugin or hook support |
| pi | any version with extension support (0.84+) |
| Go | 1.23+, to build from source |

### Setup

`remaimber import` works on its own — it reads every agent's transcripts off disk.
Wiring it into an agent adds the parts a scheduled import can't do: archiving
*before* `/compact` destroys data, capturing a session's repo identity while its
worktree still exists, and giving the agent the search tools.

#### Claude Code

```bash
remaimber setup
```

This adds the hooks (`SessionStart`, `PreCompact`, `Notification`, `SessionEnd`)
and the MCP server to `~/.claude/settings.json`. Alternatively install this repo
as a plugin — same hooks, plus the `/rmb:recall`, `/rmb:resume` and
`/rmb:sessions` commands.

#### Codex

> **Requires Codex 0.148.0 or newer.** Verified on 0.151.0.

Asynchronous command hooks arrived in 0.148.0. On 0.147 and older, Codex logs
`skipping async hooks, not supported yet` and runs the rest: the archive is still
written before a compaction, but the background maintenance sweeps and the
auto-install never fire — a failure that looks like nothing happening at all.

```bash
codex plugin marketplace add erwint/remaimber
codex plugin add rmb@remaimber
```

The plugin (`plugins/rmb/`) bundles the lifecycle hooks, the MCP server, and the
`recall` / `resume` / `sessions` skills. Codex does not trust bundled hooks on
install: run `/hooks` once and trust them, or they are silently skipped. If
`remaimber` isn't on PATH yet, the `SessionStart` hook downloads it from the
latest release.

#### pi

```bash
pi install git:github.com/erwint/remaimber
```

pi has no MCP, so the package ships the same skills written against the CLI, plus
an extension (`pi/extensions/remaimber.ts`) that hooks `session_start`,
`agent_settled`, `session_compact` and `session_shutdown`. Install `remaimber`
itself first — the extension calls it and stays quiet when it's absent.

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

## Agents

| | transcripts | resumed with | integration |
|---|---|---|---|
| Claude Code | `~/.claude/projects/<key>/<id>.jsonl` | `claude --resume <id>` (after relinking) | `remaimber setup`, or the plugin at the repo root |
| Codex | `~/.codex/sessions/<y>/<m>/<d>/rollout-*-<id>.jsonl` | `codex resume <id>` | the plugin in `plugins/rmb/` (Codex ≥ 0.148.0) |
| pi | `~/.pi/agent/sessions/<key>/<ts>_<id>.jsonl` | `pi --session <path>` | the pi package (repo root `package.json`) |

An import is retroactive: the first sweep after adding an agent picks up its
whole history, not just new sessions. Two derived layers lag behind it. Summaries
catch up on their own — the throttled sweep takes the entire backlog, newest
first — or force them now with `remaimber summarize --all`. Durable repo identity
is *not* retroactive: run `remaimber backfill-identity` once so `--repo .` finds
the older sessions (those whose cwd no longer exists stay unattributed).

Every agent shares one archive at `~/.remaimber/remaimber.db`. An archive still
sitting at the old `~/.claude/remaimber/` is moved there on first use, with a
symlink left behind so a session already running against the old path keeps
reaching the same database without a restart.

Sessions carry the agent they came from, and `remaimber resume <id>` prints the
right command for it. Searches span every agent by default; `--agent claude`,
`--agent codex` or `--agent pi` narrows `search`, `recall` and `list` to one.
The MCP tools invert that default: `search_conversations`, `find_context` and
`list_sessions` scope to the calling agent's own conversations, since an agent
asking through MCP is nearly always looking for its own earlier work — pass
`agent: "all"` to search every agent, or name one. Codex files rollouts by date rather than by project, so its
project key comes from the cwd recorded in each rollout's own header — which puts
it in the same project bucket as the Claude Code and pi sessions for that
directory. `remaimber doctor` reports, per agent, whether an installed one has
never been archived.

## Cross-worktree find & resume

Claude Code keys session storage by launch directory, so the same repo scatters across many project keys (one per worktree) and native `--resume` can't find sessions from other worktrees — or from Agent worktrees that were later deleted.

remaimber captures a **durable identity** for every session at start (a `SessionStart` hook records `repo_id` = `realpath(git --git-common-dir)` and `subpath` = `git rev-parse --show-prefix`). Because it's captured at start, it survives deletion of the worktree. You can then:

- `remaimber list --repo .` — every session for this repo, across all worktrees
- `remaimber resume <id>` — copy the session's transcript under the current directory's project key so `claude --resume <id>` works *here*, no worktree switching

If a chosen session looks like it's still running in another worktree, resume warns you (resuming a live transcript would corrupt it). Run `remaimber backfill-identity` once after upgrading to populate identity for sessions whose worktree still exists.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `REMAIMBER_DB` | `~/.remaimber/remaimber.db` | Database path. One archive for every agent |
| `REMAIMBER_LLM` | `claude` | Summary backend: `claude` (uses the local CLI) or an OpenAI-compatible base URL (e.g. `http://localhost:11434/v1` for Ollama, `http://localhost:1234/v1` for LM Studio) |
| `REMAIMBER_LLM_MODEL` | `haiku` (claude backend) | Model name for summarization |
| `REMAIMBER_LLM_KEY` | — | Optional bearer token for the HTTP backend |

### Summaries and hooks

Summaries are produced by a **throttled background sweep** (`summarize-if-stale`) wired into several hooks — `SessionStart`, `Notification`, and `SessionEnd`. It deliberately does **not** rely on `SessionEnd` alone, because that event isn't guaranteed to fire (e.g. a corporate VM killed overnight never cleanly ends its sessions). Running on `SessionStart` and `Notification` means a session left unsummarized by an unclean shutdown gets caught up the next time Claude runs. The sweep throttles itself (default 15 min) so firing it often is cheap.

The rolling summary is **offset-based and incremental**, so the sweep also checkpoints *active* sessions (not just finished ones) — each pass folds only the messages added since the last summary. That way, if the machine is killed mid-session, the latest checkpoint (at most one throttle interval old) survives on disk and the session is still recallable by its summary, not just by full-text search. (This assumes `~/.claude` is persisted across restarts, which it normally is — that's where both the archive DB and Claude's transcripts live.)

Both backends run from hooks, including inside a live Claude session:

- **Local/HTTP backend** (Ollama, LM Studio, …): a plain HTTP call, no constraints.
- **`claude` backend**: invoked as `claude -p --no-session-persistence --model haiku`. `--no-session-persistence` means the summarization call creates no session of its own, so it runs fine nested inside a Claude session and fires no lifecycle hooks (no recursion). It needs the `claude` binary and auth available in the hook's environment; where that isn't the case (headless/corporate), use the local/HTTP backend.

When a summary call fails, the reason is written to the session row and reported
by `remaimber doctor`, not just printed. The sweep runs from hooks that send
stderr to `/dev/null`, so an unrecorded failure would leave the backlog silently
stuck — indistinguishable from having nothing left to summarize. The record is
cleared as soon as that session summarizes successfully.

Summarization treats the conversation transcript as **untrusted data** — the system prompt instructs the model never to follow instructions found inside it and to reply with only the summary, guarding against prompt injection from archived content.

Liveness does not depend on a clean `SessionEnd`: a session is considered "still running" only if its transcript file was modified in the last few minutes, so a killed session correctly ages out on its own.

## How it works

Coding agents store conversations as JSONL files — `~/.claude/projects/` for Claude Code, `~/.codex/sessions/` for Codex, `~/.pi/agent/sessions/` for pi. Those files get pruned on a retention schedule and are destroyed by compaction.

remaimber archives everything into `~/.claude/remaimber/remaimber.db` with:
- **Full conversation memory** — stores all JSONL line types, not filtered
- **FTS5 search** — porter stemming, date/role/project filtering
- **UUID + content-hash dedup** — safe concurrent imports, no duplicates
- **One importer at a time** — hooks fire from every agent at once, so importers take a lock and wait briefly for each other rather than contending inside SQLite; one that cannot get in skips, since the running import scans the same files
- **Byte-offset tracking** — incremental imports skip already-processed content
- **Content cleaning** — strips system-injected XML tags from search index
