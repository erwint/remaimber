---
description: Find a past conversation and resume it in the current worktree
---

Help the user resume a previous Claude Code conversation — including ones that ran in a different git worktree — without making them switch directories.

## How sessions are identified

remaimber captures a durable identity for every session at start: `repo_id` (stable across all worktrees of a repo) and `subpath` (the monorepo sub-project). This is why a session started in a now-deleted Agent worktree is still findable.

## Steps

1. Identify the target session:
   - If `$ARGUMENTS` describes what the user was working on, call the `search_conversations` MCP tool with `repo: "."` (current repo) and the query. The results include each session's `summary`, `repo_id`, and `cwd`.
   - If `$ARGUMENTS` is empty or vague, run `remaimber resume` (no args) in the shell to list this repo's sessions across all worktrees, newest first.
2. Present the top candidates: session id (first 8 chars), subpath, branch, and summary. If one clearly matches, pick it; otherwise ask the user to choose.
3. Once a session is chosen, decide with the user how to resume:
   - **Native resume (full fidelity, restarts Claude):** call the `link_session` MCP tool with the session id (it places the JSONL under the current cwd's project key), then tell the user to run `claude --resume <id>`. If the session's branch differs from the current one, tell them to `git checkout <branch>` first.
   - **Continue here (no restart):** call `get_session` to read the past conversation, summarize what was done and what's unfinished, `git checkout <branch>` if needed, and continue the work in this session.
4. Respect the liveness warning: if `link_session` or `remaimber resume` reports the session looks **live** in another worktree, do NOT resume it — warn the user it would corrupt the transcript, and suggest closing that other session first.

## Notes

- Always prefer the session's own `cwd`/identity for any path you show — never reverse-engineer it from the project key (that encoding is lossy).
- `remaimber resume <session-id>` does the link + prints both resume options in one step; you can run it directly instead of calling `link_session`.
