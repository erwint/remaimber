# remaimber

Archive, search, and resume coding-agent conversations — Claude Code, Codex and
pi in one archive. A Go CLI and MCP server that keeps every conversation in
SQLite with FTS5 full-text search.

Agents delete their own transcripts: they prune on a retention schedule and
discard the rest on compaction. remaimber copies them into a database that
outlives them, keyed so that a conversation from one agent is findable and
resumable from another.

## Install

Two steps: the CLI, then whichever agents you use. The CLI does the work; the
agent integrations call it at the right moments.

### 1. The CLI

```bash
go install github.com/erwint/remaimber/cmd/remaimber@latest   # needs Go 1.26+
```

Or download a binary from [Releases](https://github.com/erwint/remaimber/releases)
and put it on your `PATH`.

The Claude Code and Codex plugins can also install it: their `SessionStart` hook
downloads the latest release to `~/.local/bin` when `remaimber` is missing. Hooks
and the MCP server fall back to that directory, so archiving works either way —
but add `~/.local/bin` to your `PATH` to run `remaimber` yourself.

### 2. Your agents

Optional: `remaimber import` reads every agent's transcripts off disk on its own.
Wiring an agent up adds what a periodic import cannot do — archiving *before* a
compaction destroys the context, capturing a session's repo identity while its
worktree still exists, and giving the agent the search tools.

| agent | requires | install |
|---|---|---|
| Claude Code | — | `remaimber setup`, or the plugin |
| Codex | 0.148.0+ | the plugin |
| pi | 0.84+ | the package |

**Claude Code** — either route works; the plugin adds slash commands.

```bash
remaimber setup                                   # hooks + MCP server
# or
claude plugin marketplace add erwint/remaimber
claude plugin install rmb@remaimber                # + /rmb:recall, /rmb:resume, /rmb:sessions
```

`setup` writes hooks to `~/.claude/settings.json` and registers the MCP server
with `claude mcp add --scope user`, the only place Claude Code reads user-scope
servers from — one written into `settings.json` is never loaded. Restart Claude
Code afterwards.

**Codex** — needs 0.148.0 or newer, where asynchronous command hooks landed.
Older versions log `skipping async hooks, not supported yet` and run only the
synchronous ones: the archive is still written before a compaction, but
background maintenance never fires.

```bash
codex plugin marketplace add erwint/remaimber
codex plugin add rmb@remaimber
```

Then run `/hooks` inside Codex once and trust them — Codex skips bundled hooks
until you do.

**pi** — no MCP, so the package ships the same skills written against the CLI,
plus an extension hooking `session_start`, `agent_settled`, `session_compact` and
`session_shutdown`.

```bash
pi install git:github.com/erwint/remaimber
```

`remaimber setup` configures Claude Code only; Codex and pi own their plugin and
package through their own tooling. Both `setup` and `remaimber doctor` end by
listing every agent they find, whether it is wired up, and the commands that
finish the job.

## Usage

```bash
# Import all conversations
remaimber import

# Search conversations (every agent by default)
remaimber search "sqlite configuration"
remaimber search "auth" --role user --since 2026-01-01
remaimber search "recipe import" --repo .     # this repo, across all worktrees
remaimber search "apply_patch" --agent codex  # one agent's conversations

# Recall by what the work turned out to be, not what was typed
remaimber recall 'smtp relay on the nas'

# List sessions
remaimber list
remaimber list --project myproject --json
remaimber list --repo . --subpath .           # this repo + current monorepo subpath

# Show or export a session (short ID prefixes work)
remaimber show abc123
remaimber export --last 1 --format markdown
remaimber export <session-id> --format json

# Find & resume a session in the CURRENT worktree, from any agent
remaimber resume                                # this repo's sessions, any worktree
remaimber resume <session-id>                   # print how to open it
remaimber resume --match 'the mail relay work'  # find the passage, not the session

# Move/copy a conversation to another project
remaimber move <session-id> <target-project> --copy

# Rolling summaries (LLM-backed; see Configuration)
remaimber summarize                           # sessions with new activity
remaimber summarize <session-id> --force      # rebuild one session's summary

# What is wired up, what is stuck, what failed quietly
remaimber doctor
remaimber stats

# Maintenance
remaimber verify
remaimber delete <session-id>
remaimber backfill-identity                   # repo identity for pre-existing sessions

# Shell completions
remaimber completion zsh > "${fpath[1]}/_remaimber"
```

## MCP tools

`remaimber mcp` speaks MCP over stdio. Hosts namespace the tools by server, so an
agent calls them as `mcp__remaimber__find_context` and so on.

| tool | what it is for |
|------|----------------|
| `find_context` | A topic in plain words → the stretch of *any* conversation actually about it, ranked, with summaries; messages only on request |
| `get_segments` | The passage inside one known session, or its segment list to choose from |
| `get_summary` | A session's rolling summary and per-segment summaries, without the messages |
| `search_conversations` | FTS5 search over message text |
| `get_session` | The messages of one session |
| `list_sessions` | Sessions, with filters |
| `move_conversation` | Move or copy a conversation between projects |
| `link_session` | Link a session into the current project so it can be resumed here |

`search_conversations` and `list_sessions` take `repo: "."` and `subpath: "."`,
meaning the current repo or subpath, resolved from the server's working
directory.

`find_context`, `search_conversations` and `list_sessions` also take `agent`,
which defaults to the conversations of whichever agent is calling — identified
from the MCP client name, since an agent asking through MCP is nearly always
looking for its own earlier work. Pass `agent: "all"` for every agent, or name
one. The CLI defaults the other way: it searches everything, and `--agent`
narrows it.

## Agents

| | transcripts | resumed with |
|---|---|---|
| Claude Code | `~/.claude/projects/<key>/<id>.jsonl` | `claude --resume <id>`, after relinking |
| Codex | `~/.codex/sessions/<y>/<m>/<d>/rollout-*-<id>.jsonl` | `codex resume <id>` |
| pi | `~/.pi/agent/sessions/<key>/<ts>_<id>.jsonl` | `pi --session <path>` |

Every agent shares one archive at `~/.remaimber/remaimber.db`. Each session
records which agent produced it, and `remaimber resume <id>` prints the right
command for that agent.

Sessions are grouped by project. Claude Code and pi encode the directory in their
storage path; Codex files rollouts by date instead, so its project comes from the
cwd recorded in each rollout's own header — which puts it in the same bucket as
the other agents' sessions for that directory.

Adding an agent is retroactive: the next import picks up its whole history, not
only new sessions. Summaries follow on their own, or immediately with
`remaimber summarize --all`. Repo identity does not — run
`remaimber backfill-identity` once so `--repo .` finds older sessions.

## Cross-worktree find & resume

Claude Code keys session storage by launch directory, so one repo scatters across
many project keys — one per worktree — and native `--resume` cannot see sessions
from another worktree, or from a temporary worktree that has since been deleted.

remaimber captures a **durable identity** for every session at start: a
`SessionStart` hook records `repo_id` (`realpath(git --git-common-dir)`, identical
across every worktree of a repo) and `subpath` (`git rev-parse --show-prefix`).
Captured at the start, it survives the worktree. So:

- `remaimber list --repo .` — every session for this repo, whatever worktree it ran in
- `remaimber resume <id>` — link that transcript under the current directory's project key, so `claude --resume <id>` works *here*

Resume warns when the chosen session looks like it is still running elsewhere;
resuming a live transcript corrupts it. Liveness is judged by the transcript's
modification time rather than a clean `SessionEnd`, so a killed session ages out
by itself.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `REMAIMBER_DB` | `~/.remaimber/remaimber.db` | Database path. One archive for every agent |
| `REMAIMBER_LLM` | `claude` | Summary backend: `claude` (the local CLI) or an OpenAI-compatible base URL (`http://localhost:11434/v1` for Ollama, `http://localhost:1234/v1` for LM Studio) |
| `REMAIMBER_LLM_MODEL` | `haiku` (claude backend) | Model used for summarization |
| `REMAIMBER_LLM_KEY` | — | Bearer token for the HTTP backend |

### Summaries

A long conversation is summarized in **segments**, so it can be recalled — or
resumed in part — without reading all of it. Summaries come from a throttled
background sweep wired into recurring hooks (`SessionStart`, `Notification`,
`SessionEnd` and their equivalents), deliberately not `SessionEnd` alone: that
event is not guaranteed to fire, and a machine killed overnight would leave its
sessions unsummarized forever. The sweep throttles itself (15 minutes by
default), so firing it often costs nothing.

The rolling summary is offset-based and incremental, so the sweep also
checkpoints *active* sessions — each pass folds in only what was added since the
last one. A session interrupted by a crash is still recallable from a summary at
most one interval old.

Both backends run from hooks, including inside a live session:

- **HTTP backend** (Ollama, LM Studio, …): a plain HTTP call, no constraints.
- **`claude` backend**: `claude -p --no-session-persistence --model haiku`.
  `--no-session-persistence` means the call creates no session of its own, so it
  nests inside a Claude session without firing lifecycle hooks or recursing. It
  needs the `claude` binary and its auth in the hook's environment; where that is
  not available (headless, corporate), use the HTTP backend.

A failed summary is recorded on the session and reported by `remaimber doctor`.
The sweep runs from hooks that discard stderr, so a failure that was only printed
would leave the backlog stuck with no visible reason. The record clears on the
next success.

Summarization treats the transcript as **untrusted data**: the system prompt
tells the model never to follow instructions found inside it and to reply with
the summary alone, so archived content cannot inject its way into a summary.

## How it works

Agents store conversations as JSONL — `~/.claude/projects/` for Claude Code,
`~/.codex/sessions/` for Codex, `~/.pi/agent/sessions/` for pi. Each format is
parsed by its own scanner into one shared shape, so search, summarization and
resume behave the same whichever agent a conversation came from.

The archive at `~/.remaimber/remaimber.db` keeps:

- **Every JSONL line type**, not a filtered subset
- **FTS5 search** with porter stemming and date/role/project/agent filters
- **UUID and content-hash dedup**, so a re-import cannot duplicate anything
- **Byte-offset tracking**, so an import reads only what is new
- **One importer at a time** — hooks fire from several agents at once, so importers take a lock and wait briefly instead of contending inside SQLite; one that cannot get in skips, because the running import covers the same files
- **Cleaned text** — each agent's injected scaffolding is stripped from the search index, so a search matches conversation rather than boilerplate
