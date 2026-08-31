---
name: recall
description: Search archived Codex, Claude Code and pi conversations by keyword, topic, or date. Use when the user refers to something discussed or decided in an earlier session, asks "when did we...", "what did we do about...", or wants to find a past conversation.
---

# Recall

Find something in the user's archived conversations. The archive spans every
agent that has been imported — Codex, Claude Code and pi — so a conversation is
findable from whichever one the user is in now.

Everything below is `remaimber`, a CLI on PATH. If this agent also exposes the
remaimber MCP tools (`find_context`, `get_segments`, `get_session`), prefer them
for the first two lookups: same data, fewer round trips, and they return
structured results.

## Pick the right lookup for what they remember

Three lookups exist, and they fail in different ways. Choose by what the user
actually said.

**They remember roughly what happened, not the wording** — "the part where we
set up a mail relay on the nas", "when we fixed the flaky tests":

```
remaimber resume --match 'the part where we set up a mail relay on the nas'
```

This is the default for a described memory. It searches every conversation and
ranks *passages* — contiguous stretches actually about the topic — returning each
with its session, time span and segment summaries. It handles prose, filler words
and hyphenation, and it excludes the live session, so the current conversation
cannot rank its own discussion of the topic above the one being looked for. The
MCP equivalent is `find_context` with their phrasing as `topic`.

**They remember an exact string** — an error message, a flag, a function name:

```
remaimber search 'exit status 1' --limit 10
```

Matches literal message text.

**They remember an outcome but no phrasing at all** — "when did we sort out the
DHCPv6 stalls?":

```
remaimber recall 'dhcpv6 stalls'
```

Searches segment *summaries* rather than raw text, so it matches what the work
turned out to be even when nobody typed those words together.

## Scoping to one agent

The archive spans agents, and the two entry points default differently. The MCP
tools (`find_context`, `list_sessions`, `search_conversations`) search **this
agent's own conversations** unless told otherwise — pass `agent: "all"` to search
every agent, or name one (`claude`, `codex`, `pi`). The CLI is the other way
round: it searches everything, and `--agent <name>` narrows it.

So when a lookup comes back empty, widening the agent scope is the first thing to
try, not the last: the work may well have happened in another agent.

## Filters

All three accept scoping. Add them when the user implies a scope:

- `--repo .` restricts to the current repo across every worktree; `--subpath .`
  narrows to the current sub-project
- `--agent claude|codex|pi` restricts to one agent (the CLI searches all of them by default)
- `--project <name>` filters by project key
- `--since <date>` / `--until <date>` for date ranges (ISO 8601, e.g. `2026-08-06T11:18`)
- `--role user` / `--role assistant` (search only)

`search` skips tool output by default — command and file output is machine noise,
and it includes remaimber's own archived results, so leaving it in makes a search
match earlier searches for the same term. Pass `--include-tool-output` only when
the user is specifically looking for something a command printed.

## Presenting results

Give session ID (first 8 chars), timestamp, project, and the matching snippet. A
leading `*` means resumable. Sessions from another agent are tagged with its name
next to the project; mention that when it matters, since resuming one means
opening that agent.

Search results carry a `segment_seq` — mention it when offering to pull that part
back, since it feeds straight into `remaimber resume <id> --segments <seq>`.

If nothing comes back, say so plainly and check `remaimber stats` for summary
coverage before concluding the conversation isn't archived — an unsummarized
session is invisible to `recall` and to segment lookups, though `search` still
finds it.
