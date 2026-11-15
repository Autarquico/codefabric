# Code Flock

Code Flock (`cf`) is a local-first control plane for running, moving, observing and comparing coding agents (Claude Code, Codex CLI, …) across a dynamic pool of machines.

> *Tasks belong to Code Flock; Runs happen on Workers.*

You think in **Projects → Tasks → Results**. Code Flock takes care of **Workers → Workflows → Harnesses → Models → Logs → Scheduling**.

## Status

Pre-release. The latest stable is `v0.2.1`; the `v0.4.0` betas carry the newest work. What works today:

- A Controller you run yourself, with Workers on macOS and Linux joined through a wizard and kept up as launchd or systemd services.
- Tasks that run as isolated worktrees on a Worker, with checkpoints, a journal per Worker and backups you can restore on another host.
- Handing a Claude Code or Codex session over to a remote Run (`handto`).
- A CLI and a TUI; signed releases and a verifying installer.

Next: attaching to a live Run from your terminal ([#179](https://github.com/Autarquico/codeflock/issues/179)) and seeing remote Runs from Claude Code ([#180](https://github.com/Autarquico/codeflock/issues/180)).

## Install

```bash
curl -fsSL https://github.com/Autarquico/codeflock/releases/latest/download/install.sh | sh
```

Then follow the [getting started guide](docs/guide/getting-started.md).

## Documentation

- Guide: [`docs/guide/`](docs/guide/)
- Specification: [`docs/codefabric-spec.md`](docs/codefabric-spec.md)
- Roadmap: §43 of the spec (Phases 0–10, v0.1 → v1.0)
- Architecture decisions: [`docs/adr/`](docs/adr/)

## Build

Requires Go (see `go.mod`) and, for integration tests, `git` and `tmux`.

```bash
make build          # bin/cf, static, no cgo
make test           # unit tests
make lint           # golangci-lint v2, including layering rules
make cross          # linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
```

```bash
./bin/cf version
```

## License

[Apache-2.0](LICENSE)
