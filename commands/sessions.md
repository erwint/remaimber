---
description: List recent conversation sessions with summaries, including those from worktrees
---

List archived Claude Code sessions using `remaimber list`, showing each session's summary.

## What to run

- Inside a git repo, default to the current repo across all its worktrees:
  `remaimber list --repo . --limit 20`
  Add `--subpath .` to narrow to the current monorepo sub-project.
- Otherwise: `remaimber list --limit 20`.
- Pass any user-specified filters as flags (`--project`, `--since`, `--until`).

Use `--json` to get structured output — each entry includes `summary`, `repo_id`,
`subpath`, `worktree_root`, `git_branch`, and `message_count`.

## Cross-worktree awareness

Sessions started in git worktrees (e.g. Agent-tool temp worktrees) are stored
under a different project key. remaimber records a durable `repo_id`
(`realpath(git --git-common-dir)`) per session that is identical across every
worktree of a repo — that is the reliable correlator, not the branch. `--repo .`
uses it to gather every session for the current repo regardless of worktree.

## Presenting

Show a row per session: resumable indicator (`*` = its transcript still exists),
session id (first 8 chars), repo subpath or project, message count — and the
**summary** as the description (fall back to the first prompt only when a session
has no summary yet). If results span worktrees, note which is which via `cwd` /
`worktree_root`.
