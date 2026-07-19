# Tuskbase

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Local-first repo memory for AI coding agents.**

Coding agents are getting good at changing code. They are still bad at remembering why the code is the way it is.

Tuskbase gives agents a shared memory for a repository: decisions, rationale, constraints, conflicts, and lookup receipts. It is built for the moment before an agent edits code, when it should ask: what has this repo already decided?

```text
attach -> lookup -> preflight -> remember
```

Tuskbase stores decisions, not chat logs. The goal is to make repo context durable enough that the next agent session does not have to rediscover the same architecture, conventions, and tradeoffs from scratch.

## Why

A common agent failure mode is not bad syntax. It is drift.

One session decides that auth belongs at the daemon boundary. Another session, missing that context, adds bearer-token handling inside every MCP client config. Both changes may look reasonable in isolation. Together, they turn local setup into a mess.

Tuskbase makes that kind of drift explicit:

| Step | Question | Result |
|---|---|---|
| `attach` | Which repo am I working in? | Workspace identity and local docs context. |
| `lookup` | What decisions matter before I edit? | Active decisions, claims, evidence, constraints, and receipts. |
| `preflight` | Does my plan follow or fight prior direction? | Follows, extends, duplicates, supersedes, or conflicts. |
| `remember` | What did we decide after the work? | Durable decision record with rationale and evidence. |

For MCP-connected coding agents, the preferred path is higher level: agents call `tuskbase_prepare_change` before editing and `tuskbase_finish_change` after verification. The primitive loop remains available for manual use and custom clients.

## What Works Today

Tuskbase is in its first implementation slice. The current product surface is usable locally, with clear deferred areas.

Implemented now:

- Go CLI for setup, diagnostics, daemon lifecycle, and local key management.
- MCP tools for automatic prepare/finish workflow plus `attach`, `context`, `lookup`, `check`, `preflight`, `remember`, `assess`, structured decision query, conflict resolution, reconciliation, stats, recent decisions, active conflicts, and reviewable repo-document imports.
- Local Basic daemon mode with SQLite, loopback HTTP MCP, and stdio bridge auth.
- Local Shared mode with Postgres selected from Docker-managed pgvector Postgres or an existing pgvector-enabled Postgres DSN.
- Semantic active-memory lookup with pgvector when embeddings are configured, deterministic text fallback, preflight conflict detection, lookup receipts, and optional OpenAI or Ollama embeddings.
- Local bearer auth for HTTP MCP/REST, auth-derived actor attribution, and named Local Shared agent keys.
- Authenticated local control API for daemon-backed CLI operations, including document import review.
- `tuskbase doctor` and bridge diagnostics for common local setup failures.
- Compressed local disaster-recovery backups for Local Basic SQLite and Docker-managed Local Shared Postgres, with manual create/list/restore commands and automatic write-triggered retention.
- Optional REST API for local development/debugging.

Deferred:

- UI.
- SDKs.
- Package-manager wrappers and release-channel polish.

## Install From Source

For normal setup, use an installed or stable built `tuskbase` binary. Autostart service installation intentionally refuses temporary `go run` build artifacts.

```bash
go build -o ~/.local/bin/tuskbase ./cmd/tuskbase
tuskbase version
```

If `~/.local/bin` is not on your `PATH`, either add it or choose another stable install path.

For repository development or preview-only commands, `go run` is still useful:

```bash
go run ./cmd/tuskbase version
go run ./cmd/tuskbase setup --print-only
```

## Quick Start

Recommended first setup is Local Basic: one local daemon, SQLite storage, MCP bridge auth, no Docker.

```bash
tuskbase setup
tuskbase connect codex --apply
tuskbase status
```

The default MCP client config uses `tuskbase bridge`, so Codex, Claude, Cursor, and other local clients do not need `TUSKBASE_API_KEY` exported in every shell.

After setup, work normally in your coding agent. Compliant MCP-connected agents should automatically prepare before editing, stop when Tuskbase returns `should_edit=false`, verify their work, and finish with a durable decision only when the work created or changed a repo decision. Routine summaries and chat logs should not be remembered.

Print client-specific setup commands:

```bash
tuskbase connect codex
tuskbase connect claude
tuskbase connect cursor
```

Codex may show `Auth: Unsupported` for Tuskbase. That is expected for the default stdio bridge path: Codex launches `tuskbase bridge`, and the bridge authenticates to the local daemon internally. It does not mean daemon auth is disabled. Check the active policy with:

```bash
tuskbase status
```

## Setup Modes

| Mode | Store | Best For | Infrastructure |
|---|---|---|---|
| Local Basic | SQLite | One developer using local agents on one machine | Local daemon |
| Local Shared | Postgres + pgvector extension | Heavier local multi-agent use or shared local memory | Docker or existing Postgres |

Local Basic is the default:

```bash
tuskbase setup
```

Local Shared with Docker-managed pgvector Postgres:

```bash
tuskbase setup --mode local-shared --yes
```

Local Shared with your own database:

```bash
tuskbase setup --mode local-shared --postgres-source existing --postgres-dsn postgres://tuskbase:secret@localhost:5432/tuskbase?sslmode=disable
```

All Local Shared Postgres paths require the `vector` extension. The Docker path provisions it. Existing Postgres must allow `CREATE EXTENSION IF NOT EXISTS vector` or have pgvector enabled already. Semantic pgvector retrieval is active when `TUSKBASE_EMBEDDING_PROVIDER` is `ollama` or `openai`; without embeddings, Tuskbase falls back to deterministic text search.

Local semantic retrieval with Ollama:

```bash
ollama pull nomic-embed-text
export TUSKBASE_EMBEDDING_PROVIDER=ollama
export TUSKBASE_EMBEDDING_MODEL=nomic-embed-text
tuskbase daemon restart
```

For the full setup matrix, Docker context notes, and inspectable templates, see [Product Modes](docs/03_product_tiers.md#current-setup-paths) and [Local Shared Troubleshooting](docs/03_product_tiers.md#local-shared-troubleshooting).

## Daily Commands

```bash
tuskbase status
tuskbase doctor
tuskbase daemon restart
tuskbase backup create
tuskbase backup list
tuskbase auth list
tuskbase auth rotate
tuskbase auth add --name windsurf --role agent
tuskbase auth rotate --name codex
tuskbase import scan --repo .
tuskbase import list --workspace <workspace-id>
```

Manual HTTP/environment-variable setup is available for developers and CI with `--transport http`. `TUSKBASE_AGENT_KEYS` takes precedence over `TUSKBASE_API_KEY`; stored setup config is used when neither env var is set. See [.env.example](.env.example).

## Backup And Restore

Tuskbase stores compressed backups outside Docker volumes by default, under the local Tuskbase data area. Automatic backups run after successful durable decision, conflict, conflict-resolution, and assessment writes. Lookup receipts do not trigger backups. The latest 20 automatic backups are retained by default; manual backups are never pruned automatically.

```bash
tuskbase backup create
tuskbase backup list
tuskbase daemon stop
tuskbase backup restore /path/to/backup.tar.gz --yes
tuskbase daemon restart
```

Restore refuses to run while the daemon is reachable. Pass `--stop-daemon` to request a service stop first. SQLite restore writes a timestamped safety copy of the current database. Docker-managed Local Shared restore uses `pg_restore --clean --if-exists`. Existing Postgres DSNs are intentionally excluded from V1 backup automation; use the database provider's backup tooling.

Backups contain Tuskbase memory, not API keys, MCP client configuration, Docker credentials, or other local secrets. Configure behavior with `TUSKBASE_BACKUP_DIR`, `TUSKBASE_BACKUP_AUTO=false`, and `TUSKBASE_BACKUP_AUTO_RETENTION`.

## Troubleshooting

Start with:

```bash
tuskbase doctor
tuskbase status
```

If an MCP client reports that Tuskbase closed during initialization, check `tuskbase doctor` before debugging client config. For Docker-managed Local Shared setups, common causes are:

- Docker Desktop or Docker Engine is not running.
- Postgres is not reachable on the configured loopback port.
- An existing Docker volume has a stored database password that no longer matches Tuskbase config.

Newer Tuskbase builds report these as `store_runtime`, `postgres_connect`, and repair hints in `doctor` output. The bridge also exposes a `tuskbase_diagnostics` tool when daemon-backed MCP tools cannot be reached.

For Docker-managed Local Shared, Docker/Postgres is a runtime dependency too. Local Basic does not require Docker.

Docker volume loss can be recovered from a Tuskbase backup with `tuskbase backup restore <file> --yes --stop-daemon`. Check `tuskbase doctor` for the backup directory and the last automatic backup error.

## Current Interfaces

### MCP

| Tool | Purpose |
|---|---|
| `tuskbase_prepare_change` | Preferred pre-edit workflow: attach the repo, load context, recent decisions, open conflicts, task lookup, and plan preflight when a plan is supplied. Returns `should_edit=false` for conflicts. |
| `tuskbase_finish_change` | Preferred post-work workflow: report summary, changed files, tests, and optionally remember a durable decision. Skips memory writes when no decision is supplied. |
| `tuskbase_attach` | Attach or refresh repo workspace context. |
| `tuskbase_context` | Return a compact workspace briefing with docs, active decisions, open conflicts, recent supersessions, degraded states, and recommended next actions. |
| `tuskbase_lookup` | Retrieve relevant active decisions, claims, evidence, and docs before editing, with a lookup receipt. |
| `tuskbase_check` | Run a non-mutating proposal check with relationship evidence and conflict previews. |
| `tuskbase_preflight` | Classify whether a proposal follows, extends, duplicates, supersedes, or conflicts with active decisions, recording lookup receipts and open conflicts. |
| `tuskbase_remember` | Store a completed decision with rationale, evidence, claims, and relationships. |
| `tuskbase_assess` | Append feedback to a decision without rewriting the decision record. |
| `tuskbase_query` | Query decisions by text, type, status, confidence, and relationship filters. |
| `tuskbase_resolve_conflict` | Resolve, dismiss, or defer an existing conflict with an append-only note. |
| `tuskbase_reconcile` | Record a reconciliation decision and close the specified conflicts. |
| `tuskbase_stats` | Return aggregate trail-health stats for decisions, conflicts, assessments, and completeness. |
| `tuskbase_recent` | Show recent active decisions for a workspace. |
| `tuskbase_conflicts` | Show active conflicts for a workspace. |
| `tuskbase_import_scan` | Scan known repo documentation and persist reviewable decision candidates. |
| `tuskbase_import_list` | List imported decision candidates, defaulting to pending. |
| `tuskbase_import_accept` | Accept one candidate as an active decision with document evidence. |
| `tuskbase_import_reject` | Reject one candidate while preserving review history. |

Conflict resolution and superseding prior decisions require explicit user approval. A preflight conflict is a hard stop for editing until the user approves a changed plan, a resolution, or a reconciliation decision.

### Repo Document Import

The importer scans conservative repo documentation sources such as `README.md`, `AGENTS.md`, `CLAUDE.md`, top-level Markdown, `docs/**/*.md`, `adrs/**/*.md`, Cursor and Windsurf rules, and GitHub Copilot instructions. It creates persisted candidates, not active memory. A candidate affects lookup and preflight only after `tuskbase import accept <candidate-id>` or `tuskbase_import_accept` stores it through the normal `remember` path with `document` evidence.

V1 extraction is offline and rule-based: ADR-like documents, explicit "we use/choose/require/avoid/do not" language, and durable repo rules. Optional Ollama-assisted extraction is future work.

```bash
tuskbase import scan --repo .
tuskbase import list --workspace <workspace-id>
tuskbase import show <candidate-id>
tuskbase import accept <candidate-id>
tuskbase import reject <candidate-id> --summary "Too broad"
```

### Optional REST API

The REST API is not mounted by the default Local Basic daemon. Enable it explicitly only for local development/debugging:

```bash
tuskbase serve --http-mcp --rest
```

| Capability | Purpose |
|---|---|
| Attach workspace | Create or update repo workspace context. |
| Workspace context | Return compact repo context, active decisions, conflicts, and next actions. |
| Lookup memory | Retrieve relevant repo memory with a receipt. |
| Check proposal | Evaluate a proposal without mutating receipts or conflicts. |
| Preflight proposal | Evaluate a proposal before an agent acts and record open conflicts. |
| Remember decision | Record a final decision. |
| Assess decision | Append feedback to a decision. |
| Query decisions | Filter decisions by text, type, status, confidence, and relationships. |
| Resolve conflict | Resolve, dismiss, or defer a conflict. |
| Reconcile conflicts | Record a reconciliation decision and close conflicts. |
| Trail stats | Report decision, conflict, assessment, and completeness health. |
| Recent decisions | List recent decisions for a workspace. |
| Active conflicts | List active conflicts for a workspace. |
| Health and status | Report local server, adapter, and indexing health. |

## Architecture Direction

Tuskbase is interface-first. The product is not Postgres, pgvector, Qdrant, Kafka, a Go package, an MCP SDK, a UI framework, an SDK, or any single embedding provider. Those are adapters.

The intended shape is:

```text
HTTP API / MCP / later UI / later SDKs / optional support CLI
  -> application use cases
    -> domain model
      -> infrastructure interfaces
        -> replaceable adapters
```

Canonical decision writes must not depend on derived indexes, embeddings, or network services. SQLite is the default local adapter because the first experience should be easy to run on a developer machine. Postgres is available behind the same boundaries for Local Shared setups. Search indexes are derived and rebuildable. Indexing failures must not cause a decision to be lost.

## Contributing

Tuskbase is early, so good contributions are especially concrete:

- Keep docs honest about what exists today.
- Prefer local-first flows that work without cloud services.
- Keep domain/application behavior independent from infrastructure adapters.
- Add focused tests beside changed code.
- Run the default offline suite before broad changes:

```bash
go test ./...
```

For setup or daemon work, also run:

```bash
go test ./cmd/tuskbase ./internal/daemon
```

See [Agent Guide](AGENTS.md) for repo-specific development instructions.

## Docs

| Document | Purpose |
|---|---|
| [Product Brief](docs/00_product_brief.md) | Product identity, target users, core loop, and non-goals. |
| [Architecture](docs/01_architecture.md) | Layering, interfaces, flows, and anti-drift rules. |
| [Technology Direction](docs/02_tech_stack.md) | Current technology defaults and adapter boundaries. |
| [Product Modes](docs/03_product_tiers.md) | Local Basic and Local Shared product mode direction. |
| [Auth And Security](docs/04_auth_security.md) | Local auth, identity, and security direction. |
| [Security](SECURITY.md) | Vulnerability reporting and current security posture. |
| [Changelog](CHANGELOG.md) | Release notes and notable project changes. |

## License

Apache 2.0. See [LICENSE](LICENSE).
