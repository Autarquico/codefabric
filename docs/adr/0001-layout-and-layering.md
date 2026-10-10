# ADR 0001 — Package layout and layering

- Status: accepted
- Date: 2026-10-10

## Context

The spec (§32) proposes a package layout and states that `domain` must not import adapters, UI or storage, and that CLI, TUI and MCP call the same operations (invariant 7). Without enforcement these rules erode.

## Decision

Follow §32, adding only what the implementation plan needs:

| Package | Purpose |
|---------|---------|
| `internal/api/v1` | Wire types and error envelope shared by the API server and the client SDK, so the SDK never imports the Controller |
| `internal/worker/state` | The Worker's durable local state (`state.db`, §6.4) |
| `internal/worker/gitops` | Worker-side git: bare clones, worktrees, sealing, push (§22.5–22.6) |
| `internal/controller/relay` | The git relay (§22.4) |
| `internal/support/*` | Domain-free utilities: clock, atomic files, redaction, canonical JSON |
| `internal/testutil/*`, `internal/e2e` | Test doubles and end-to-end tests |

Packages are created when the first code needs them, not ahead of time.

Layering is enforced with depguard in `.golangci.yml`:

- `domain`: standard library and `ulid` only.
- `app`: no adapter implementations, HTTP layer or UIs.
- `cli`, `tui`, `mcp`: no `app`, `controller`, `worker` or `adapters`.

The daemon commands (`cf controller run`, `cf worker run`) need the Controller and Worker packages. They are wired in `cmd/cf`, which composes the client command tree from `internal/cli` with daemon commands exposed by `internal/controller` and `internal/worker`. `internal/cli` stays a pure client.

## Consequences

- A layering violation fails `make lint` and CI.
- `cmd/cf` is the only place where client and server code meet.
