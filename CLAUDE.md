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
- `internal/homedir/` — home resolution that survives a scrubbed environment
- `internal/mover/` — move/copy conversations between projects
- `internal/setup/` — Claude Code configuration, and the per-agent status report
- `internal/types/` — shared type definitions

## Agent integrations

- `.claude-plugin/` + `commands/` + `hooks/` + `.mcp.json` — Claude Code plugin
  (repo root is the plugin root)
- `plugins/rmb/` + `.agents/plugins/marketplace.json` — Codex plugin; its
  `skills/` are shared with pi
- `package.json` (`pi` manifest) + `pi/extensions/` — pi package

Bump `version` in `plugins/rmb/.codex-plugin/plugin.json` when the Codex plugin
changes: Codex caches an installed plugin by version. Both plugins have a
validator worth running — `claude plugin validate .` and the plugin-creator
skill's `validate_plugin.py` for Codex.

`remaimber setup` wires up every agent it finds (`internal/setup/agents.go`) by
running that agent's own plugin install — the plugin carries hooks, MCP and
skills, so there is nothing left for setup to write. `--agent` limits it,
`--dry-run` prints without doing, `--no-plugin` takes the older Claude Code route
(hooks in settings.json plus `claude mcp add`). Both routes at once write the
hooks twice, so setup refuses the second. `doctor` prints the same per-agent
report.

**Claude Code does not read `mcpServers` from `settings.json`.** User-scope MCP
servers live in `~/.claude.json`, which `claude mcp add --scope user` owns — that
file also holds Claude Code's own state, so it is not ours to rewrite. A server
written into settings.json is silently inert, which is how the search tools went
missing for months without a symptom other than an agent saying the tool does not
exist.

There is no Codex equivalent of `commands/`: custom prompts are deprecated and
live only in `~/.codex/prompts`, so nothing shippable can provide a slash
command. Skills are the sanctioned replacement — `$rmb:recall` mentions one
directly, and the model can load it implicitly from its description.

The Codex plugin needs Codex ≥ 0.148.0, where asynchronous command hooks landed.
Below that the async hooks are skipped with `skipping async hooks, not supported
yet`. Dropping `"async": true` for shell backgrounding (`... &`, as the Claude
Code hooks do) would remove the floor, at the cost of Codex no longer accounting
for the work it started.

## Release

```bash
./scripts/release.sh v0.2.0
git push origin main && git push origin v0.2.0
```
