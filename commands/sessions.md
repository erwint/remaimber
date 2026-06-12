---
description: List recent conversation sessions, including those from worktrees
---

List recent archived Claude Code sessions using `remaimber list`.

## Cross-worktree awareness

Sessions started in git worktrees (used by the Agent tool) are stored under a different project key derived from the worktree's temp path, not the original project. They won't appear when filtering by the current project.

To find sessions across worktrees, use the `--json` flag and look at the `git_branch` field — this is the most reliable correlator since worktrees share the same branch name as the original checkout.

If `$ARGUMENTS` is empty, run `remaimber list --limit 20`. Otherwise pass arguments as flags.

Present results as a table: resumable indicator (`*`), session ID (first 8 chars), project, title/first prompt, message count.
