# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

---

## Frame Project Management

This project is managed with **Frame**. Follow the rules below to keep documentation up to date.

### Task Management (tasks.json)

**These ARE TASKS - add to tasks.json:**
- Feature requests or changes ("Let's do this", "Let's add this", "Improve this")
- Deferred work ("We'll do this later", "Let's leave it for now")
- Gaps or improvement opportunities discovered while coding
- Bug fixes needed

**These are NOT TASKS:** error messages, questions, temporary experiments, completed work, instant typo fixes.

**Task Creation Flow:** Detect task patterns → ask user "I identified these tasks from our conversation, should I add them to tasks.json?" → if approved, add with this structure:
```json
{
  "id": "unique-id",
  "title": "Short and clear title",
  "description": "Detailed explanation",
  "status": "pending | in_progress | completed",
  "priority": "high | medium | low",
  "context": "Where/how this task originated",
  "createdAt": "ISO date",
  "updatedAt": "ISO date",
  "completedAt": "ISO date | null"
}
```

When starting a task: `status: "in_progress"`. When complete: `status: "completed"`, update `completedAt`. After commit: update related task statuses.

### PROJECT_NOTES.md

Update when: architectural decisions are made, technology choices are made, important problems are solved, or an approach is determined with the user. Format: `### [YYYY-MM-DD] Title` — add to the Session Notes section at the end. Don't summarize — record the conversation as-is with context.

Ask the user "Should I add this conversation to PROJECT_NOTES.md?" when: a task completes successfully, an important architectural/technical decision is made, a noteworthy bug is fixed, or "let's do this later" is said. Don't ask for every small change.

### STRUCTURE.json

Update when: a new file/folder is created or moved, module dependencies change, or an important architectural pattern is discovered.

### General Rules

- **Language:** English for all documentation
- **Date Format:** ISO 8601 (YYYY-MM-DDTHH:mm:ssZ)
- **After Commit:** Check tasks.json and STRUCTURE.json
- **Session Start:** Review pending tasks in tasks.json

---

## Project: parse-dmarc

Go application that fetches DMARC aggregate reports from an IMAP mailbox, parses them, stores them in SQLite, and serves a Vue.js dashboard. Also runs as an MCP server for AI assistant integration.

## Commands

```bash
# Install all dependencies (Go + Node via Bun)
just install-deps

# Build full app (frontend → embed → Go binary)
just build            # CGO_ENABLED=0, pure-Go SQLite
just build-cgo        # CGO_ENABLED=1, mattn/go-sqlite3

# Development
just dev              # hot reload via air (Go only)
just frontend-dev     # Vite dev server only

# Test
go test -v ./...
go test -v ./internal/parser/...   # single package

# Lint
golangci-lint run

# Generate sample config
just config           # writes config.json via go run . -gen-config
```

## Architecture

### Package Layout

`main.go` is a thin entry point — all CLI logic lives in `cmd/server/`:
- `cmd/server/command.go` — defines the `*cli.Command` with all flags (urfave/cli/v3)
- `cmd/server/run.go` — the main action: loads config, starts storage/IMAP/API/metrics
- `cmd/server/mcp.go` — MCP-specific startup path

`internal/` packages are independent and injected via constructors:
- `config` — loads from JSON file + environment (caarlos0/env)
- `storage` — SQLite via `database/sql`; driver selected by build tag
- `parser` — pure XML parsing of DMARC RFC 7489 `<feedback>` documents (handles gzip, zip, raw XML)
- `imap` — email fetching; attaches DMARC report attachments to the parser
- `api` — HTTP server with embedded frontend; routes defined inline in `server.go`
- `mcp` — MCP server using modelcontextprotocol/go-sdk (stdio + HTTP/SSE transports)
- `metrics` — Prometheus metrics, registered globally, used as middleware in `api`

### Build Tag SQLite Selection

Two files implement the same `NewStorage(path string)` constructor via build tags:
- `internal/storage/sqlite_no_cgo.go` — build tag `//go:build !cgo`, uses `modernc.org/sqlite` (pure Go)
- `internal/storage/sqlite_cgo.go` — build tag `//go:build cgo`, uses `mattn/go-sqlite3`

`just build` passes `CGO_ENABLED=0`; `just build-cgo` passes `-tags cgo`.

### Frontend Embedding

`bun run build` → `dist/` → `cp -a dist internal/api/` → Go embeds `internal/api/dist` via `//go:embed`. The binary is fully self-contained and serves the SPA from memory.

### Database Schema

Two tables: `reports` (report metadata + raw JSON blob) and `records` (per-record data per report). Reports are deduplicated via `INSERT OR IGNORE` on `report_id`.

### Config Priority

Flags → environment variables → JSON config file. The `--config` flag (or `PARSE_DMARC_CONFIG` env var) points to the JSON file; missing keys fall through to `caarlos0/env` defaults.

## Code Style

- Go: `gofmt` + golangci-lint; structured logging via `rs/zerolog` (use the package-level `log` variable from `cmd/server/command.go`)
- Frontend: Vue 3 Composition API, Prettier; custom reactive store pattern in `src/stores/` (similar to Pinia but without the dependency)
- Pre-commit hooks: `.pre-commit-config.yaml`
