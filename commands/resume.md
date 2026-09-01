---
description: Find a past conversation and resume it in the current worktree, including one from another agent
---

Help the user resume a previous conversation — including ones that ran in a different git worktree, or in a different agent — without making them switch directories.

## How sessions are identified

remaimber captures a durable identity for every session at start: `repo_id` (stable across all worktrees of a repo) and `subpath` (the monorepo sub-project). This is why a session started in a now-deleted Agent worktree is still findable.

## Resume the part, not the whole thing

Sessions here run to thousands of messages. When the user names a topic — "resume X, but only the part where we did Y" — do **not** load the whole conversation. Find the passage instead.

Prefer the `find_context` MCP tool when the conversation itself is uncertain, and `get_segments` when a session id is already known. The host namespaces them, so they are `mcp__remaimber__find_context` and `mcp__remaimber__get_segments`; a lookup for the bare name finds nothing:

- `find_context` takes `topic` in plain words and searches every conversation, returning ranked passages with session, time span and segment summaries. Use it first when the user describes work without naming a session.
- `get_segments` takes `session_id` plus `match` and locates the passage inside that one conversation. Without `match`, it lists every segment so you can choose by summary.

Both return summaries first and messages only on `include_messages: true`. Read the summaries, decide which passage is right, *then* pay for the text. Both also return alternatives — a one-word topic is often ambiguous, so if the top passage looks wrong, check the runners-up before widening the search.

Equivalent shell forms, if you prefer them:

```
remaimber resume --match 'the part where we set up a mail relay on the nas'   # any conversation
remaimber resume b2bd8168 --match 'smtp relay'                                # within one
remaimber resume b2bd8168 --segments 4 --print                                # explicit part
remaimber resume b2bd8168 --since 2026-08-06T11:18 --until 2026-08-06T11:45   # by time
```

## Steps

1. Identify the target:
   - Topic described, session unknown → `find_context` with their phrasing.
   - Session known, part wanted → `get_segments` with `session_id` and `match`.
   - Nothing specific → run `remaimber resume` (no args) to list this repo's sessions across all worktrees, newest first.
2. Present the top candidates: session id (first 8 chars), subpath, branch, time span, and summary. If one clearly matches, pick it; otherwise ask.
3. Decide with the user how to resume:
   - **Partial (usual case):** load the chosen passage's messages, summarize what was done and what's unfinished, `git checkout <branch>` if needed, and continue here. No restart.
   - **Native full resume:** run `remaimber resume <session-id>` and hand the user the command it prints — `claude --resume <id>` for a Claude Code session (that command links the JSONL under the current cwd's project key first), `codex resume <id>`, or `pi --session <path>`. Check the branch first. Note this always resumes the *whole* session — partial resume is a way of reading part of it as context, not a smaller transcript.
4. Respect the liveness warning: if `remaimber resume` reports the session looks **live** in another worktree, do NOT resume it — warn that it would corrupt the transcript, and suggest closing that session first.

## Notes

- Always prefer the session's own `cwd`/identity for any path you show — never reverse-engineer it from the project key (that encoding is lossy).
- If a session has no segments it was never summarized; `get_segments` will say so. Fall back to `get_session`, or suggest `remaimber summarize <id>`.
