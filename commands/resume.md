---
description: Find a previous conversation to resume, searching across worktrees
---

Help the user find and resume a previous Claude Code conversation.

## Steps

1. Run `remaimber list --limit 20` to get recent sessions for the current project.
2. If `$ARGUMENTS` is not empty, also run `remaimber search "$ARGUMENTS" --limit 10` to find sessions matching the user's description across ALL projects (this catches worktree sessions stored under different project keys).
3. From the combined results, show only sessions marked with `*` (resumable — their JSONL file still exists on disk). Skip non-resumable sessions unless highly relevant.
4. Present the top candidates with: session ID (first 8 chars), project name, title/first prompt.
5. Once the user picks a session (or if there's a clear best match), provide the resume command:

```
claude --resume <full-session-id>
```

## Cross-worktree sessions

Sessions from Agent worktrees are stored under a project key derived from the worktree's temp path, not the original project. If the best match is from a different project key, warn the user and suggest copying it first:

```
remaimber move --copy <session-id> <current-project-key>
```

The current project key can be derived from `$CWD` by replacing `/` with `-` (e.g., `/Volumes/Data/src/foo` becomes `-Volumes-Data-src-foo`).
