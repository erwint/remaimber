---
name: resume
description: Find a past conversation and resume it in the current worktree, including sessions that ran in another agent or another git worktree. Use when the user wants to continue, pick up, or reopen earlier work.
---

# Resume

Help the user resume a previous conversation — including one that ran in a
different git worktree, or in a different agent — without making them switch
directories or hunt for a session id.

Everything below is `remaimber`, a CLI on PATH. If this agent also exposes the
remaimber MCP tools (`find_context`, `get_segments`, `get_session`), prefer them
for locating the passage: same data, structured results.

Hosts namespace MCP tools by server, so they appear as
`mcp__remaimber__find_context`, `mcp__remaimber__get_segments` and so on. The
bare names used below are the short form; if a lookup for the bare name finds
nothing, the tool is probably there under the namespaced one.

## How sessions are identified

remaimber captures a durable identity for every session at start: `repo_id`
(stable across all worktrees of a repo) and `subpath` (the monorepo sub-project).
This is why a session started in a now-deleted temporary worktree is still
findable.

Each session also records the agent that produced it. That decides how it opens:

| agent  | native resume                 |
|--------|-------------------------------|
| codex  | `codex resume <session-id>`   |
| claude | `claude --resume <session-id>` |
| pi     | `pi --session <path>`         |

`remaimber resume <session-id>` prints the right one — use it rather than
assembling the command yourself, since Claude Code sessions also need their
transcript linked under the current directory first, which that command does.

## Resume the part, not the whole thing

Sessions here run to thousands of messages. When the user names a topic —
"resume X, but only the part where we did Y" — do **not** load the whole
conversation. Find the passage instead.

```
remaimber resume --match 'the part where we set up a mail relay on the nas'   # any conversation
remaimber resume b2bd8168 --match 'smtp relay'                                # within one
remaimber resume b2bd8168 --segments 4 --print                                # explicit part
remaimber resume b2bd8168 --since 2026-08-06T11:18 --until 2026-08-06T11:45   # by time
```

The MCP equivalents are `find_context` (topic in plain words, searches every
conversation) and `get_segments` (`session_id` plus `match`, locates the passage
inside one conversation; without `match` it lists every segment so you can choose
by summary). Both return summaries first and messages only on
`include_messages: true` — read the summaries, decide which passage is right,
*then* pay for the text. Both also return alternatives: a one-word topic is often
ambiguous, so if the top passage looks wrong, check the runners-up before
widening the search.

## Steps

1. Identify the target:
   - Topic described, session unknown → `remaimber resume --match '<their phrasing>'`.
   - Session known, part wanted → `remaimber resume <id> --match '<topic>'`.
   - Nothing specific → run `remaimber resume` with no arguments to list this
     repo's sessions across all worktrees and agents, newest first.
2. Present the top candidates: session id (first 8 chars), agent, subpath, branch,
   time span, and summary. If one clearly matches, pick it; otherwise ask.
3. Decide with the user how to resume:
   - **Partial (usual case):** load the chosen passage's messages, summarize what
     was done and what's unfinished, `git checkout <branch>` if needed, and
     continue here. No restart, and it works even when the session came from
     another agent.
   - **Native full resume:** run `remaimber resume <session-id>` and hand the user
     the command it prints. Check the branch first. Note this always resumes the
     *whole* session — partial resume is a way of reading part of it as context,
     not a smaller transcript.
4. Respect the liveness warning: if `remaimber resume` reports the session looks
   **live** elsewhere, do NOT resume it — warn that it would corrupt the
   transcript, and suggest closing that session first.

## Notes

- Always prefer the session's own `cwd`/identity for any path you show — never
  reverse-engineer it from the project key (that encoding is lossy).
- If a session has no segments it was never summarized; `get_segments` will say
  so. Fall back to `get_session`, or suggest `remaimber summarize <id>`.
