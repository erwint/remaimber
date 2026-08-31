# remaimber

Go CLI + MCP server for archiving coding-agent conversations (Claude Code, Codex,
pi) into SQLite with FTS5.

## Build & Test

```bash
make build    # builds to bin/remaimber
make install  # installs to ~/.local/bin/remaimber
make test     # runs all tests
```

## Project Structure

- `cmd/remaimber/` — CLI entry point (cobra), MCP server
- `internal/db/` — SQLite connection, schema, queries
- `internal/importer/` — JSONL scanning, parsing, importing (one file per agent:
  `parser.go`/`scanner.go` for Claude Code, `pi.go`, `codex.go`)
- `internal/mover/` — move/copy conversations between projects
- `internal/setup/` — Claude Code settings.json configuration
- `internal/types/` — shared type definitions

## Agent integrations

- `.claude-plugin/` + `commands/` + `hooks/` — Claude Code plugin (repo root is
  the plugin root)
- `plugins/rmb/` + `.agents/plugins/marketplace.json` — Codex plugin; its
  `skills/` are shared with pi
- `package.json` (`pi` manifest) + `pi/extensions/` — pi package

Bump `version` in `plugins/rmb/.codex-plugin/plugin.json` when the Codex plugin
changes: Codex caches an installed plugin by version.

## Release

```bash
./scripts/release.sh v0.2.0
git push origin main && git push origin v0.2.0
```
