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
| `internal/app/reconciliation` | How the Controller and its Workers agree again after a restart or a silence (§27): the start of the Controller and the gate of the scheduler, `POST /v1/worker/reconcile`, and the Runs that are lost (§27.2) |
| `internal/app/events`, `internal/app/stream` | Reading the event store (§20): pages with a snapshot-consistent cursor, and the follow that keeps delivering; the pacing of quiet streams |
| `internal/worker/state` | The Worker's durable local state (`state.db`, §6.4) |
| `internal/worker/executor`, `journal`, `execwrap`, `procident` | What the Worker does with a Run: assignments, supervision and stop (§6.4, §10.7); the journal of reports it sends (§20.4); the process wrapper `cf worker exec` and its marker files; telling a process from a reused pid |
| `internal/adapters/runtime`, `internal/adapters/harness` | Ports of the process sessions (§6.1) and of the harnesses (§7.2), with their implementations in subpackages (`runtime/tmux`, `harness/command`) |
| `internal/adapters/harness/localhost`, `harness/command`, `harness/harnesstest` | The `harness.Host` of this machine, apart from the port so that the Worker daemon and, later, the client (adopt) compose it; the generic harness `command`; the contract suite every harness adapter runs in its tests (§42.3) |
| `internal/adapters/secrets`, `secrets/file` | Port of the Worker's local secret store (`Store{List, Get, Set, Delete}`, §16.7) and its `file` backend over `<config>/worker/secrets.json` |
| `internal/worker/host` | What the Worker does with its harnesses besides running them: materialising the configuration of a Run in its run directory (§8.12) and announcing the harnesses it offers (§10.1) |
| `internal/worker/outputship` | Sends the output of the Runs of this Worker to the Controller in batches (#14); it reads `output.log` safely and does not know the executor |
| `internal/worker/secretjit` | Just-in-time delivery of a Run's secrets to its run directory and their recoverable release (#23): the wrapper resolves refs from the delivery, and the redaction and the publication guard keep the values until capture and publication close |
| `internal/adapters/runtime/remote`, `internal/adapters/transport/ssh` | The command that attaches to a session from another machine, per runtime (it cannot live in the port: the port cannot import `tmux`), and the ssh Transport (§5.4) that runs it |
| `internal/worker/gitops` | Worker-side git: bare clones, worktrees, sealing, push (§22.5–22.6) |
| `internal/controller/relay` | The git relay (§22.4): bare repositories, the CGI adapter of `git http-backend` and the `pre-receive` hook (also `cf relay hook`, composed in `cmd/cf`) |
| `internal/support/*` | Domain-free utilities: clock, atomic files, redaction, canonical JSON |
| `internal/support/treedigest` | `treedigest/v1`, the content digest of a directory tree; standard library only |
| `internal/testutil/*`, `internal/e2e` | Test doubles and end-to-end tests |
| `internal/testutil/fakeharness` | A scripted harness program and its adapter, for tests of the harness port, the executor and the scheduler |

Packages are created when the first code needs them, not ahead of time.

Layering is enforced with depguard in `.golangci.yml`:

- `domain`: standard library and `ulid` only.
- `app`: no adapter implementations, HTTP layer or UIs.
- `cli`, `tui`, `mcp`: no `app`, `controller`, `worker` or `adapters`.
- `worker` (tests excepted): no `controller` or `app`. The Worker reaches the Controller only through `internal/client`.
- `adapters` (tests excepted): no `controller`, `app` or `worker`. An adapter implements a port and knows nothing of who calls it.
- `cli` (tests excepted): only `client`, `api/v1`, `domain`, `config`, `buildinfo` and `cobra`/`x/sys` among the packages of the module and its dependencies. The terminal of `cf attach` comes from `cmd/cf` through `cli.Options`.
- `worker/outputship` (tests excepted): no `executor`; it reaches the Controller through an interface the SDK satisfies.
- `controller/relay` (tests excepted): no `app`, `controller/api` or `controller/store`; the use cases reach it through a port (`runs.Relay`). `worker/gitops` (tests excepted): no `executor`, `state` or `client`; the executor reaches it through a port (`executor.Git`).
- `testutil` and `adapters/harness/harnesstest` are for tests: no production code imports them. `harnesstest` (tests excepted) imports no `worker`, `app`, `controller` or `testutil`.
- `adapters/harness`, the port itself (tests excepted): none of its implementations (`localhost`, `command`, `harnesstest`).
- `worker/host` (tests excepted): no `executor`, `state` or `client`; the executor calls it, never the other way round.
- `support/treedigest` (tests excepted): the standard library only; `treedigest/v1` is a format.
- `worker/secretjit` (tests excepted): no `executor`, `state`, `client` or `execwrap`; the executor calls it, and the wrapper takes its resolver as a function.
- `adapters/secrets`, the port itself (tests excepted): not its implementation `file`. Like every adapter, `adapters/secrets/**` imports no `worker`, `app` or `controller`.
- `controller` and `app`, tests included: no `adapters/secrets` or `worker/secretjit`. The Controller holds metadata of secrets, never a value.
- `support/redact` (tests excepted): the standard library only.

The daemon commands (`cf controller run`, `cf worker run`) need the Controller and Worker packages. They are wired in `cmd/cf`, which composes the client command tree from `internal/cli` with daemon commands exposed by `internal/controller` and `internal/worker`. `internal/cli` stays a pure client.

## Consequences

- A layering violation fails `make lint` and CI.
- `cmd/cf` is the only place where client and server code meet.
