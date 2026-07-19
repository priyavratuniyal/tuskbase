# Tuskbase Product Brief

Tuskbase is local-first, repo-aware shared memory and decision history for AI coding agents. It exists so Codex, Claude Code, Cursor, Windsurf, and similar tools can stop re-learning the same repo and stop contradicting prior decisions.

## Product Identity

Tuskbase is repo-aware memory and decision history for AI coding agents. It gives agents a shared local layer to check before acting and update after acting.

Technically:

```text
local-first repo memory + temporal decisions + vector retrieval + conflict engine
```

The product is not generic memory. The center of gravity is governed decision memory: what was decided, why, by whom, in which workspace, against which prior context, and whether a new proposal contradicts active direction.

## Primary Pain

The main problem is not that agents forget. The deeper problem is hidden drift:

```text
Agent A makes a decision.
Agent B never sees it.
Agent C contradicts it.
The human discovers the mess days later.
```

Tuskbase turns that hidden drift into an explicit workflow:

```text
look up context -> preflight proposal -> remember final decision -> detect conflicts
```

For compliant MCP-connected coding agents, the preferred workflow is automatic:

```text
prepare_change before edits -> edit only when should_edit=true -> finish_change after verification
```

## Product Wedge

The wedge is not "a better notes app." The wedge is decision hygiene for AI coding agents.

The first lovable use case:

- Attach a repo once.
- Ask an agent to work.
- The agent checks prior decisions before making changes.
- The agent records meaningful decisions after work.
- The next tool sees the same context.

## Core Loop

```text
attach -> lookup -> preflight -> remember
```

Each part has a specific job:

- `attach`: understand the workspace and repo context.
- `lookup`: retrieve prior decisions, claims, repo documents, active conflicts, and constraints.
- `preflight`: evaluate a proposal before committing to it.
- `remember`: store the final decision with evidence and relationships.

These are primitives. The preferred agent path is `tuskbase_prepare_change` before editing and `tuskbase_finish_change` after verification. The high-level tools compose the primitives so users can ask for normal coding work while compliant agents check memory automatically.

## Primary Product Surfaces

The first product surfaces are:

- local HTTP API server,
- local MCP server.
- local disaster-recovery backup and restore commands for Tuskbase-owned stores.

The API and MCP server should expose the same core loop through shared application use cases. The first implementation should be a Go local service that can run both surfaces from one process.

A UI comes after the API and MCP flows are useful. SDKs come after the core contracts are stable. The CLI should stay focused on setup, diagnostics, daemon operation, and local auth administration rather than becoming the product center.

## Core User Journeys

### Attach A Repo

The user or agent attaches a workspace through the local API or MCP server.

Tuskbase scans useful repo files and creates or updates a workspace profile. It should detect stack, conventions, architecture constraints, external services, and important prior notes.

The importer can also scan known repo documentation and create reviewable decision candidates. These imported candidates are separate from canonical decisions until a user accepts one candidate at a time. Acceptance stores the candidate through the normal remember path with source-document evidence; pending or rejected candidates are not active guidance.

### Ask For Context

The user or agent sends a lookup request:

```text
query: "password reset Redis"
```

Tuskbase returns relevant decisions, claims, repo document chunks, active conflicts, and a lookup receipt.

### Check Before Acting

An agent should call the high-level prepare workflow before editing:

```text
tuskbase_prepare_change
```

The response includes context, recent decisions, open conflicts, lookup results, optional preflight, a verdict, `should_edit`, and next actions. If `should_edit=false`, the agent stops before file edits and asks the user how to proceed.

The lower-level primitives remain available for manual use and custom clients:

```text
tuskbase_context
tuskbase_check
tuskbase_lookup
tuskbase_preflight
```

The response should answer:

- What did we already decide?
- Does this proposal follow, extend, supersede, duplicate, or conflict with that?
- What should the agent do next?

Conflict resolution and superseding an active decision require explicit user approval before the agent records a resolution, reconciliation, or superseding decision.

### Remember After Acting

After verification, an agent calls:

```text
tuskbase_finish_change
```

If no durable decision was made, the finish workflow reports the work summary and skips memory writes. If a durable decision is supplied, it stores the decision through the same remember path. The stored decision includes outcome, reasoning, confidence, evidence, considered options, extracted claims, and graph relationships.

### Continue Across Tools

Later phases add handoff generation.

The output summarizes completed work, pending work, relevant decisions, changed files, and constraints for the next agent.

## Non-Goals

Do not turn Tuskbase into:

- a generic chatbot memory database,
- a notes app,
- a task manager,
- a wiki replacement,
- a dashboard-first product,
- an organization governance suite,
- a cloud sync platform.

Generic facts may be stored only when they support repo context, decisions, claims, or handoff.

## Product Principles

- Decision memory beats raw note storage.
- Workspace scope is mandatory.
- The conflict engine is the differentiator.
- Search is a retrieval layer, not the source of truth.
- Canonical records live behind replaceable store interfaces.
- Vector indexes are rebuildable.
- Local canonical memory should have inspectable backups outside the primary SQLite file or Docker volume.
- Local-first value is the product boundary.
- API and MCP should work before UI polish or SDKs.
- Agents need concise, action-oriented output.
- Humans need clear lineage, not decorative UI.

## Success Criteria

Phase 1 is credible when:

- a repo can be attached,
- a decision can be remembered,
- lookup retrieves that decision,
- preflight catches the Redis conflict example,
- preflight does not mislabel compatible Postgres token decisions as conflicts,
- the local Go service hosts the API and MCP surfaces through the same application core,
- API and MCP expose the same core loop plus assessment, structured query, conflict resolution, reconciliation, stats, and compact workspace context through the same application core,
- default tests run without external embedding services.
