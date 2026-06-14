---
description: List recent conversation sessions, including those from worktrees
---

List archived Claude Code sessions using `remaimber list`.

## Cross-worktree awareness

Sessions started in git worktrees are stored under a different project key (derived from the worktree path), so they don't show up when filtering by the current project. remaimber records a durable `repo_id` per session that is identical across every worktree of a repo.

To list every session for the current repo regardless of worktree:
```
remaimber list --repo . --limit 20
```
Add `--subpath .` to narrow to the current monorepo sub-project.

If `$ARGUMENTS` is empty, default to `remaimber list --repo . --limit 20` when inside a git repo, otherwise `remaimber list --limit 20`. Pass any user-specified filters as flags.

Present results as a table: resumable indicator (`*`), session id (first 8 chars), subpath/project, title or summary, message count. Use `--json` if you need the `repo_id`, `subpath`, or `worktree_root` fields.
