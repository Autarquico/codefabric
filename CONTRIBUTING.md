# Contributing

## Workflow

1. Every non-trivial change starts from a spec in `docs/specs/<slug>.md` with Given/When/Then scenarios, linked to an issue.
2. Branch from `main`, open a PR that closes the issue.
3. Each scenario of the spec has at least one test named `TestScenario_<Slug>_<Description>`.
4. CI must be green: lint, unit (Linux and macOS, with `-race`), integration, cross-build, govulncheck.

The product specification is [`docs/codefabric-spec.md`](docs/codefabric-spec.md). Changes that contradict it update the spec in the same PR.

## Layering

Enforced by depguard (`make lint`):

- `internal/domain` imports only the standard library and `ulid`.
- `internal/app` depends on ports, never on adapter implementations, the HTTP layer or UIs.
- `internal/cli`, `internal/tui` and `internal/mcp` go through `internal/client` only (spec invariant 7).

## Tests

| Kind | How to run | What is real |
|------|------------|--------------|
| Unit | `make test` | pure logic |
| Integration | `make test-integration` (tag `integration`) | SQLite, git, tmux, TLS |
| End-to-end | `make test-e2e` (tag `e2e`) | compiled `cf` binary, multiple processes |

Tests must not depend on wall-clock time: inject the clock.

## Migrations

SQLite migrations are numbered by phase to avoid collisions between parallel milestones: Phase 0 uses `0001–0099`, Phase 1 `0100–0199`, and so on. Each milestone spec reserves its range.

## Commits

Small, focused commits with imperative messages. No generated files in commits except `go.sum`.
