---
name: sessions
description: List recent conversation sessions with summaries, across agents and git worktrees. Use when the user asks what they worked on recently, or wants to see or pick from their past sessions.
---

# Sessions

List archived sessions using `remaimber list`, showing each session's summary.
The archive spans every imported agent — Codex, Claude Code and pi.

## What to run

- Inside a git repo, default to the current repo across all its worktrees:
  `remaimber list --repo . --limit 20`
  Add `--subpath .` to narrow to the current monorepo sub-project.
- Otherwise: `remaimber list --limit 20`.
- Pass any user-specified filters as flags (`--project`, `--since`, `--until`).

Use `--json` to get structured output — each entry includes `summary`, `agent`,
`repo_id`, `subpath`, `worktree_root`, `git_branch`, and `message_count`.

## Scoping to one agent

The archive spans agents, and the two entry points default differently. The MCP
tools (`find_context`, `list_sessions`, `search_conversations`) search **this
agent's own conversations** unless told otherwise — pass `agent: "all"` to search
every agent, or name one (`claude`, `codex`, `pi`). The CLI is the other way
round: it searches everything, and `--agent <name>` narrows it.

So when a lookup comes back empty, widening the agent scope is the first thing to
try, not the last: the work may well have happened in another agent.

## Cross-worktree awareness

Sessions started in git worktrees (temporary agent worktrees, for instance) are
stored under a different project key. remaimber records a durable `repo_id`
(`realpath(git --git-common-dir)`) per session that is identical across every
worktree of a repo — that is the reliable correlator, not the branch. `--repo .`
uses it to gather every session for the current repo regardless of worktree.

Codex files its rollouts by date rather than by project, so its project key is
derived from the cwd its own header records — which puts a Codex session in the
same project bucket as the Claude Code and pi sessions for that directory.

## Presenting

Show a row per session: resumable indicator (`*` = its transcript still exists),
session id (first 8 chars), repo subpath or project, message count — and the
**summary** as the description (fall back to the first prompt only when a session
has no summary yet). Sessions from an agent other than Claude Code are tagged
with the agent name next to the project; keep that visible, since it decides how
the session is resumed. If results span worktrees, note which is which via `cwd`
/ `worktree_root`.

## If sessions look missing or summaries are stale

`remaimber stats` reports summary coverage — an unsummarized session is invisible
to `recall` and to segment lookups. `remaimber doctor` checks the things that fail
quietly: hooks not firing, imports gone stale, an installed agent whose sessions
have never been archived, summaries missing from the search index, and sessions
whose last summary attempt errored (which is why a backlog can sit still). Suggest
`remaimber summarize --all` to catch up, or `remaimber summarize --reindex` when
summaries exist but aren't searchable (that one makes no model calls).
