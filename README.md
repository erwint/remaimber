# remaimber

Archive, search, and resume coding-agent conversations - Claude Code, Codex and
pi in one archive. A Go CLI and MCP server that keeps every conversation in
SQLite with FTS5 full-text search.

Agents delete their own transcripts: they prune on a retention schedule and
discard the rest on compaction. remaimber copies them into a database that
outlives them, keyed so that a conversation from one agent is findable and
resumable from another.

## Install

Two routes, and either one is complete on its own.

### From an agent

Install the plugin - for pi, the package - and then **start a new session**.
That is the whole setup: plugins load at startup, and the first session installs
the CLI if you do not already have it.

```bash
# Claude Code - adds /rmb:recall, /rmb:resume, /rmb:sessions
claude plugin marketplace add erwint/remaimber
claude plugin install rmb@remaimber

# Codex 0.148.0+ - then, in the new session, run /hooks once and trust them
codex plugin marketplace add erwint/remaimber
codex plugin add rmb@remaimber

# pi 0.84+
pi install git:github.com/erwint/remaimber
```

### From the CLI

Install `remaimber` yourself and let it install those plugins for you, into
every agent on the machine:

```bash
go install github.com/erwint/remaimber/cmd/remaimber@latest   # needs Go 1.26+
                                                              # or grab a release binary
remaimber setup                            # every agent it finds
remaimber setup --agent codex --dry-run    # one of them, or just look
```

Same result as running the commands above by hand - each agent installs its own
plugin, which is where the hooks and the search tools live. Restart an agent
after wiring it up.

For Claude Code without a marketplace plugin, `remaimber setup --no-plugin`
writes hooks to `~/.claude/settings.json` and registers the MCP server with
`claude mcp add --scope user`, the only place Claude Code reads user-scope
servers from. Use one route or the other: both write the same hooks, and both
firing means every event runs twice - which is why setup declines to add them
when a plugin already does.

`remaimber doctor` reports the same picture at any time: which agents are
installed, which are wired up, and what finishes the job.

### Requirements

| | |
|---|---|
| Claude Code | any version with plugin or hook support |
| Codex | **0.148.0+** - asynchronous command hooks. Older versions log `skipping async hooks, not supported yet` and run only the synchronous ones, so the archive is still written before a compaction but background maintenance never fires |
| pi | 0.84+; 1.0+ for the MCP search tools |
| Go | 1.26+, to build from source |
| ssh / AWS CLI | only for `remaimber sync`: ssh that logs in without a prompt, or the `aws` CLI for S3 |

Nothing above is required to archive: `remaimber import` reads every agent's
transcripts off disk on its own. Wiring an agent up adds what a periodic import
cannot do - archiving *before* a compaction destroys the context, capturing a
session's repo identity while its worktree still exists, and giving the agent the
search tools.

### Pruning

The archive only grows, and over half of it by volume is tool output - command
and file output that search already ignores. `remaimber prune` drops old content
in three grades, cheapest to lose first:

| `--mode` | drops | keeps |
|---|---|---|
| `tool-output` (default) | command and file output | the conversation, summaries, segments |
| `messages` | every message | the session and its summaries, so recall still finds it |
| `sessions` | the sessions themselves | nothing |

`--older-than` takes an age (`90d`, `18m`, `2y`) or a date. `--dry-run` measures
without dropping. SQLite keeps freed pages for reuse, so the file does not shrink
until `--vacuum` rewrites it.

Nothing returns on the next import. A pruned session is tombstoned, so a
transcript still on disk is left where it is rather than read back in - without
that, removing a session would delete the very row that records the import, and
the next sweep would undo the prune. `remaimber forget <id>` lifts a tombstone,
and `remaimber delete` leaves one for the same reason.

For automatic pruning, set `REMAIMBER_RETENTION` (an age) and optionally
`REMAIMBER_PRUNE_MODE`. The background sweep then applies it once a day. Unset,
nothing is ever dropped: the archive exists to outlive the agents' own retention.

### Staying current

`remaimber update` replaces the binary with the newest release. The background
sweep also checks once a day and updates in place, because neither install route
does it otherwise: a plugin updates itself through its marketplace, while its
install hook only runs when the CLI is *missing*. A binary built from source is
left alone - it has no release version to compare against.

The version is read from the redirect on the releases page, so only `github.com`
needs to be reachable and no API quota is spent; the API is a fallback, and
`GITHUB_TOKEN` raises its limit if you land there. `HTTP_PROXY`, `HTTPS_PROXY`
and `NO_PROXY` are honoured. When a lookup fails, the error quotes what the
server said - a bare 403 hides whether it is a rate limit, a proxy denial or a
moved repository.

### Where the CLI ends up

On the plugin route, the first session downloads `remaimber` into a directory
your `PATH` already has - `~/.local/bin`, `~/bin`, or a writable
`/usr/local/bin` - so it is runnable straight away.

If none of those is on your `PATH`, it installs to `~/.local/bin` and appends one
marked line to your shell profile (`~/.zshrc` or `~/.bash_profile`), so a new
shell finds it. The line is added once and never duplicated. To keep your profile
untouched, set `REMAIMBER_NO_PATH_EDIT=1` - archiving works regardless, since the
hooks and the MCP server look in `~/.local/bin` directly.

## Using it from an agent

The three skills - **recall**, **resume**, **sessions** - are reached differently
in each agent, because each has its own convention:

| agent | how to invoke |
|---|---|
| Claude Code | `/rmb:recall`, `/rmb:resume`, `/rmb:sessions` |
| Codex | `$rmb:recall`, `$rmb:resume`, `$rmb:sessions` - or `/skills` to browse |
| pi | `/skill:recall`, `/skill:resume`, `/skill:sessions` |

In all three you can also just ask - "find the conversation where we set up the
mail relay", "what did I work on last week?" - since each skill's description is
in the model's context and it loads the right one.

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

# Update to the latest release (also checked once a day in the background)
remaimber update
remaimber update --check

# Bring in other machines' conversations (see "Syncing between machines")
remaimber sync pull ssh://build@mac-mini.local --origin mac-mini
remaimber sync pull s3://team-bucket/homes/laptop --origin laptop
remaimber list --origin mac-mini                # one machine's sessions; "local" for this one's

# Bound the archive's size (nothing is dropped unless you ask)
remaimber prune --older-than 180d --dry-run
remaimber prune --older-than 180d --vacuum

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

Each agent gets the server from what it installs: the Claude Code and Codex
plugins declare it, and on pi 1.0+ the package's extension registers it, so
there is nothing to add to any MCP configuration by hand. pi exposes it as
`deferred`: the server is listed in the system prompt, and the model loads its
tools through `tool_search` when it needs them, rather than carrying eight tool
declarations on every request. To change that, add a `remaimber` entry to
`~/.pi/agent/mcp.json`, which takes precedence over the registered one. On pi
before 1.0 the package still archives, without the tools; `remaimber doctor`
says so.

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
which defaults to the conversations of whichever agent is calling - identified
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
cwd recorded in each rollout's own header - which puts it in the same bucket as
the other agents' sessions for that directory.

Adding an agent is retroactive: the next import picks up its whole history, not
only new sessions. Summaries follow on their own, or immediately with
`remaimber summarize --all`. Repo identity does not - run
`remaimber backfill-identity` once so `--repo .` finds older sessions.

## Cross-worktree find & resume

Claude Code keys session storage by launch directory, so one repo scatters across
many project keys - one per worktree - and native `--resume` cannot see sessions
from another worktree, or from a temporary worktree that has since been deleted.

remaimber captures a **durable identity** for every session at start: a
`SessionStart` hook records `repo_id` (`realpath(git --git-common-dir)`, identical
across every worktree of a repo) and `subpath` (`git rev-parse --show-prefix`).
Captured at the start, it survives the worktree. So:

- `remaimber list --repo .` - every session for this repo, whatever worktree it ran in
- `remaimber resume <id>` - link that transcript under the current directory's project key, so `claude --resume <id>` works *here*

Resume warns when the chosen session looks like it is still running elsewhere;
resuming a live transcript corrupts it. Liveness is judged by the transcript's
modification time rather than a clean `SessionEnd`, so a killed session ages out
by itself.

## Syncing between machines

`remaimber sync pull` brings conversations from other machines into this
archive, so search and recall cover work done anywhere. What moves is the
agents' own transcript files, not database rows: each pulled file is imported by
the same rules as a local one.

A location is a path over ssh or a prefix in S3, and remaimber reads it in the
agents' native session layout. By default the path is a home directory, with
sessions where each agent keeps them: `.claude/projects`, `.codex/sessions`,
`.pi/agent/sessions` (agents missing there are skipped). With `--agent`, the
path is that one agent's session directory itself, for sessions kept or copied
anywhere else.

```bash
# Another machine, read directly over ssh (default path: the remote home)
remaimber sync pull ssh://build@mac-mini.local --origin mac-mini

# Sessions something else copies into S3: a home per machine...
remaimber sync pull s3://team-bucket/homes/laptop --origin laptop --aws-profile work
# ...or one agent's directory on its own
remaimber sync pull s3://team-bucket/laptop/codex-sessions --agent codex --origin laptop
```

Over ssh nothing needs installing on the other machine, but ssh must log in
without a prompt (a key or an agent); a host that would ask for a password fails
at once. S3 goes through the AWS CLI, so credentials resolve as they do for
`aws s3 ls`: `--aws-profile`, else `AWS_PROFILE`, else the default chain. Only
the agents' session directories are listed, so a bucket holding whole home
backups costs no more to sync than one holding only sessions.

A pull always needs `--origin`, naming the machine the sessions came from. They
show as `@origin` in `list` and `search`, and `--origin` filters by it (`local`
for this machine's own). A session already in the archive - recorded here, or
pulled under another name - is left as it is.

Each object's etag is recorded, so a repeat pull transfers only what changed:
the S3 ETag, or size and modification time over ssh. `--force` ignores them,
and `remaimber sync status` lists every origin with its last sync.

`--dry-run` (pull and push) transfers nothing and lists every file it looked at,
with what it would do and why:

```
  fetch  new               39 KB  claude/-Users-build/7d5d7031-....jsonl
  fetch  changed           46 KB  codex/2026/09/03/rollout-...-01a0645d-....jsonl
  skip   unchanged        515 KB  codex/2026/09/03/rollout-...-01a0642c-....jsonl
  skip   already local     12 KB  claude/-Users-me-proj/11111111-....jsonl
```

A push skips as `forgotten` any session pruned or forgotten here. A session
already in the archive under another origin is skipped without being
downloaded, since its file name says whose it is.

A pulled session can be read here (`remaimber resume <id> --match ...`,
`get_segments`) but resumed only on its own machine, where its transcript lives.
Pulled sessions join the summary backlog like local ones, a few at a time. A
one-off machine whose sessions are pulled later can set `REMAIMBER_LLM=off`, so
they are summarized once, by the machine that pulls them.

### Pushing to S3

When nothing else copies a machine's sessions into S3, `sync push` does, in the
same native layout, so another machine pulls them like any other copy. Give each
machine its own prefix.

```bash
remaimber sync push s3://team-bucket/homes/laptop
remaimber sync push s3://team-bucket/laptop/codex-sessions --agent codex
```

A push sends only this machine's own transcripts - never what it pulled - and
holds back any session pruned or forgotten here. Unchanged transcripts are
skipped by etag, tracked per destination. Over ssh there is nothing to push: run
pull on the other machine.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `REMAIMBER_DB` | `~/.remaimber/remaimber.db` | Database path. One archive for every agent |
| `REMAIMBER_LLM` | `claude` | Summary backend: `claude`, `codex` or `pi` (the local CLI), an OpenAI-compatible base URL (`http://localhost:11434/v1` for Ollama, `http://localhost:1234/v1` for LM Studio), or `off` to not summarize on this machine |
| `REMAIMBER_LLM_MODEL` | `haiku` (claude backend) | Model used for summarization |
| `REMAIMBER_LLM_KEY` | - | Bearer token for the HTTP backend |
| `REMAIMBER_RETENTION` | - | Age after which the daily sweep prunes (`180d`, `2y`). Unset means keep everything |
| `REMAIMBER_PRUNE_MODE` | `tool-output` | What that sweep drops: `tool-output`, `messages` or `sessions` |

### Summaries

A long conversation is summarized in **segments**, so it can be recalled - or
resumed in part - without reading all of it. Summaries come from a throttled
background sweep wired into recurring hooks (`SessionStart`, `Notification`,
`SessionEnd` and their equivalents), deliberately not `SessionEnd` alone: that
event is not guaranteed to fire, and a machine killed overnight would leave its
sessions unsummarized forever. The sweep throttles itself (15 minutes by
default), so firing it often costs nothing.

The rolling summary is offset-based and incremental, so the sweep also
checkpoints *active* sessions - each pass folds in only what was added since the
last one. A session interrupted by a crash is still recallable from a summary at
most one interval old.

`REMAIMBER_LLM` picks who does the summarizing. Any of the three agent CLIs will,
using the auth it already has, or an OpenAI-compatible endpoint:

| `REMAIMBER_LLM` | runs | notes |
|---|---|---|
| `claude` (default) | `claude -p --no-session-persistence --model haiku` | reports its own cost, so spend is tracked |
| `codex` | `codex exec --ephemeral --skip-git-repo-check` | answer read from `--output-last-message` |
| `pi` | `pi -p --no-session --no-tools` | |
| a base URL | one HTTP call | e.g. `http://localhost:11434/v1` for Ollama |
| `off` | nothing | sessions are archived and searchable, but not summarized here |

Each runs from hooks, including inside a live session of the same agent. The
ephemeral flags matter for more than tidiness: a persisted summarization session
would be imported as a conversation of its own, so the archive would fill with
its own summaries. Codex and pi report no price, so their calls are counted at
zero - the same treatment a self-hosted model gets. Where no CLI auth is
available (headless, corporate), use the HTTP backend.

`off` is for a machine whose sessions are summarized somewhere else - typically a
one-off machine another one pulls from with `remaimber sync`, which would
otherwise pay for the same summaries twice. Set it in the environment the agents
run in, since the hooks inherit it. Nothing is attempted and nothing is recorded
as failed; `remaimber summarize` says it is off rather than running, and
`doctor` reports it as a note, not as a stuck backlog.

A failed summary is recorded on the session and reported by `remaimber doctor`.
The sweep runs from hooks that discard stderr, so a failure that was only printed
would leave the backlog stuck with no visible reason. The record clears on the
next success.

Summarization treats the transcript as **untrusted data**: the system prompt
tells the model never to follow instructions found inside it and to reply with
the summary alone, so archived content cannot inject its way into a summary.

## How it works

Agents store conversations as JSONL - `~/.claude/projects/` for Claude Code,
`~/.codex/sessions/` for Codex, `~/.pi/agent/sessions/` for pi. Each format is
parsed by its own scanner into one shared shape, so search, summarization and
resume behave the same whichever agent a conversation came from.

The archive at `~/.remaimber/remaimber.db` keeps:

- **Every JSONL line type**, not a filtered subset
- **FTS5 search** with porter stemming and date/role/project/agent filters
- **UUID and content-hash dedup**, so a re-import cannot duplicate anything
- **Byte-offset tracking**, so an import reads only what is new
- **One importer at a time** - hooks fire from several agents at once, so importers take a lock and wait briefly instead of contending inside SQLite; one that cannot get in skips, because the running import covers the same files
- **Cleaned text** - each agent's injected scaffolding is stripped from the search index, so a search matches conversation rather than boilerplate

## License

MIT - see [LICENSE](LICENSE).
