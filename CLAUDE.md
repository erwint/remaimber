# remaimber

Go CLI + MCP server for archiving Claude Code conversations into SQLite with FTS5.

## Build & Test

```bash
make build    # builds to bin/remaimber
make install  # installs to ~/.local/bin/remaimber
make test     # runs all tests
```

## Project Structure

- `cmd/remaimber/` — CLI entry point (cobra), MCP server
- `internal/db/` — SQLite connection, schema, queries
- `internal/importer/` — JSONL scanning, parsing, importing
- `internal/mover/` — move/copy conversations between projects
- `internal/setup/` — Claude Code settings.json configuration
- `internal/types/` — shared type definitions

## Release

```bash
./scripts/release.sh v0.2.0
git push origin main && git push origin v0.2.0
```
