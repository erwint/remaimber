---
description: Search archived Claude Code, Codex and pi conversations by keyword, topic, or date
---

Find something in the user's archived conversations. The archive spans every agent
remaimber has imported — Claude Code, Codex and pi — so a session from any of them
is findable here.

## Pick the right tool for what they remember

Three lookups exist, and they fail in different ways. Choose by what the user actually said.

**They remember roughly what happened, not the wording** — "the part where we set up a mail relay on the nas", "when we fixed the flaky tests". Use the `find_context` MCP tool (namespaced by the host as `mcp__remaimber__find_context`) with their phrasing as `topic`. It searches every conversation, ranks passages (contiguous stretches actually about the topic), and returns each with its session, time span and segment summaries. This is the default for a described memory: it handles prose, filler words and hyphenation, and it excludes the live session so the current conversation cannot rank its own discussion of the topic.

**They remember an exact string** — an error message, a flag, a function name. Use `remaimber search`, which matches literal message text:

```
remaimber search $ARGUMENTS --limit 10
```

It excludes the live session by itself, so a search from inside a conversation
doesn't rank that conversation's own discussion of the term first.

**They remember an outcome but no phrasing at all** — "when did we sort out the DHCPv6 stalls?". Use `remaimber recall <topic>`, which searches segment *summaries* rather than raw text, so it matches what the work turned out to be even when nobody typed those words together.

## Filters

All three accept scoping. Add them when the user implies a scope:

- `--repo .` restricts to the current repo across every worktree; `--subpath .` narrows to the current sub-project
- `--agent claude|codex|pi` restricts to one agent; the CLI searches all of them by default, while the MCP tools default to this agent's own conversations and take `agent: "all"` to widen
- `--since <date>` / `--until <date>` for date ranges (ISO 8601, e.g. `2026-08-06T11:18`)
- `--role user` / `--role assistant` (search only)

`search` skips tool output by default — command and file output is machine noise, and it includes remaimber's own archived results, so leaving it in makes a search match earlier searches for the same term. Pass `--include-tool-output` only when the user is specifically looking for something a command printed.

## Presenting results

Give session ID (first 8 chars), timestamp, project, and the matching snippet. A leading `*` means resumable. Results from another agent are tagged with its name next to the project — keep that visible, since it decides how the session is resumed. Search results carry a `segment_seq` — mention it when offering to pull that part back, since it feeds straight into `remaimber resume <id> --segments <seq>`.

If nothing comes back, say so plainly and check `remaimber stats` for summary coverage before concluding the conversation isn't archived — an unsummarized session is invisible to `recall` and to segment lookups, though `search` still finds it.
