# Contributing

## Workflow

1. Every non-trivial change starts from a spec in `docs/specs/<slug>.md` (managed with `/pm`, see `AGENT.md`): context, scope, Given/When/Then scenarios and references to `docs/codefabric-spec.md`. The spec is linked to an issue on the [project board](https://github.com/orgs/Autarquico/projects/5).
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
| Integration | `make test-integration` (tag `integration`) | SQLite, git, TLS (tmux when the runtime lands) |
| End-to-end | `make test-e2e` (tag `e2e`) | compiled `cf` binary, multiple processes |

Tests must not depend on wall-clock time: inject the clock. The end-to-end suite is the exception: it runs real processes with wide margins and tests no timeout boundary (those stay in the fake-clock tests).

### End-to-end (`internal/e2e`)

A Controller, two Workers behind an L4 proxy (`internal/e2e/netproxy`) and a third client, all running the real `cf`, with real `kill -9`. Linux and macOS only. Tests that need tmux or git are skipped without them; with `CF_REQUIRE_TMUX=1` / `CF_REQUIRE_GIT=1` (as in CI) they fail instead.

| Target | What it runs |
|--------|--------------|
| `make test-e2e` | the whole suite: harness, scenarios and the property test over the seed corpus |
| `make test-e2e-property` | `TestProperty_E2eV01_Intercalados` only |
| `make test-e2e-race` | the whole suite with every `cf` of the cluster built with `-race` |
| `make e2e-reap` | runs no test; reaps the `cfe2e*` roots of the root parent that are yours alone (no link, your uid, mode `0700`/`0600`) and whose owner process is provably dead |

Make variables: `E2E_SEEDS` (random seeds after the corpus, default 0), `E2E_SEED` (exactly that seed), `E2E_TIMEOUT` (default `25m`).

| Variable | Effect |
|----------|--------|
| `CF_E2E_BIN` | `cf` binary to use instead of building one (`make test-e2e` sets `bin/cf`) |
| `CF_E2E_RACE=1` | build `cf` with `-race`; fails if `CF_E2E_BIN` is also set |
| `CF_E2E_SCALE` | factor for every time and wait of the cluster (CI uses `1.5` on macOS) |
| `CF_E2E_SEED` | run exactly this seed (`0x…` or decimal) instead of the corpus |
| `CF_E2E_SEEDS` | random seeds added after the corpus of `internal/e2e/testdata/seeds.txt` |
| `CF_E2E_REPEAT` | run each seed this many times |
| `CF_E2E_ROOT` | parent directory of the invocation's root (default `/tmp/cfe2e-<uid>`, made `0700` and refused if it is a link, of another user or open to others; short enough for the tmux socket on macOS) |
| `CF_E2E_KEEP=1` | keep the files of the invocation (processes are always killed) |
| `CF_E2E_ARTIFACTS` | directory where the manifest and logs of a failed invocation are copied, with every invite (the argument after `join`/`connect`) redacted |

To reproduce a seed, copy the line each `seed=0x…` subtest logs at its start (and again, with the trace, when it fails):

```
e2e: seed 0x825251f7b3c67c1b; to run it again: CF_E2E_SEED=0x825251f7b3c67c1b CF_REQUIRE_TMUX=1 CF_REQUIRE_GIT=1 go test -tags e2e -count=1 -run '^TestProperty_E2eV01_Intercalados$/^seed=0x825251f7b3c67c1b$' ./internal/e2e/
```

The line carries `CF_E2E_SCALE` and `CF_E2E_RACE=1` (with `-race`) when the failing run used them. A seed repeats the program of actions, not the OS interleaving; use `CF_E2E_REPEAT` to run it several times. A seed that fails because of the product goes into `seeds.txt`.

## Migrations

SQLite migrations are numbered by phase to avoid collisions between parallel milestones: Phase 0 uses `0001–0099`, Phase 1 `0100–0199`, and so on. Each milestone spec reserves its range.

A migration is immutable once it is on `main`: later changes go into a new migration. Until its PR is merged, a migration may still be edited in place, since no deployed database can have applied it.

## Commits

Small, focused commits with imperative messages. No generated files in commits except `go.sum`.
