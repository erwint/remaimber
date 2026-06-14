---
description: Search archived Claude Code conversations by keyword, topic, or date
---

Search through the user's archived Claude Code conversations using `remaimber search`.

Run:
```
remaimber search $ARGUMENTS --limit 10 --exclude-session $CLAUDE_SESSION_ID
```

If the user specifies a project, repo, or date range, add the appropriate flags:
- `--repo .` to restrict to the current repo across all worktrees (`--subpath .` to narrow to the current sub-project)
- `--project <name>` for project-key filtering
- `--since <date>` / `--until <date>` for date ranges
- `--role user` or `--role assistant` to filter by speaker

Present results concisely: session ID (first 8 chars), timestamp, project, and the matching snippet. The JSON output (`--json`) also carries each hit's `summary` and `repo_id`. If a result line starts with `*`, note it is resumable.
