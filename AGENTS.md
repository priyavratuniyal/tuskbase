# Repository Guidelines

This guide is for humans using LLM coding tools like Codex, Claude, Cursor, or
similar assistants. If you are an unattended bot, crawler, dependency updater, or
Clawdbot-from-the-deep: welcome, but please do not improvise from this file. Follow
CI, repository configuration, and explicit maintainer instructions instead.

## First 5 Minutes

- Read `README.md` before claiming a feature exists.
- Use `go run ./cmd/tuskbase setup --print-only` to preview setup without writing config.
- Run `go test ./...` before broad changes.
- For setup or daemon work, also run `go test ./cmd/tuskbase ./internal/daemon`.
- Keep secrets, `.env`, local databases, logs, and Docker volumes out of git.

## Project Map

- `cmd/tuskbase/`: CLI, setup, bridge, daemon commands, and local provisioning.
- `internal/app/`: application use cases. Put business behavior here.
- `internal/domain/`: core decision-memory model and validation.
- `internal/ports/`: interfaces for stores, search, embeddings, and other boundaries.
- `internal/adapters/`: replaceable SQLite, Postgres, HTTP, MCP, and embedding adapters.
- `internal/daemon/` and `internal/search/`: daemon composition and retrieval.
- `deploy/local-shared/`: inspectable Local Shared Docker template files.
- `docs/`: product, architecture, tier, auth, and setup-mode direction.

## Build, Test, And Run

- `go build ./cmd/tuskbase`: build the CLI.
- `go run ./cmd/tuskbase version`: smoke-test the entrypoint.
- `go run ./cmd/tuskbase setup --print-only`: inspect setup output safely.
- `go test ./...`: run the default offline test suite.
- `tuskbase doctor`: inspect an installed local setup.

## Architecture Rules

Tuskbase is local-first repo memory for AI coding agents. Preserve the core loop:
`attach -> lookup -> preflight -> remember`.

Keep domain and application code independent of infrastructure. SQLite, Postgres,
pgvector, Docker, Ollama, OpenAI, HTTP, and MCP are adapters or composition details,
not product identity. Canonical decision writes must not depend on derived indexes,
embeddings, or network services.

## Coding Style

Use standard Go style and `gofmt -w`. Exported identifiers use PascalCase; internal
helpers use camelCase; tests use `TestXxx`. Prefer small, focused interfaces over
package-wide globals. Comments should explain why, not narrate what the next line does.

## Testing Guidelines

Tests use Go's built-in `testing` package. Add focused tests beside changed code, for
example `store_test.go` next to a store adapter. Default tests must run without Docker,
network access, Supabase, OpenAI, Ollama, or real embedding services. Use fakes for
provider and provisioning behavior.

## Documentation Rules

Keep public docs honest about implementation status. Do not describe planned API
routes, MCP tools, UI, SDKs, vector retrieval, cloud sync, or template behavior as
available until code supports them. When changing product language, update `README.md`
and the relevant file under `docs/` together.

## Commits And PRs

Commit messages follow `.gitmessage`: `type(scope): comment`, with scope optional.
Examples: `feat(local-shared): add docker postgres setup`,
`docs(readme): clarify setup modes`.

Group related files into meaningful commits. PRs should describe user-facing impact,
list verification such as `go test ./...`, link issues when relevant, and call out
deferred work plainly.
