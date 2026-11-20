# Code Flock

Code Flock (`cflk`) is a local-first control plane for running, moving, observing and comparing coding agents (Claude Code, Codex CLI, …) across a dynamic pool of machines.

> *Tasks belong to Code Flock; Runs happen on Workers.*

You think in **Projects → Tasks → Results**. Code Flock takes care of **Workers → Workflows → Harnesses → Models → Logs → Scheduling**.

## Status

Pre-release. The latest stable is `v0.2.1`; the `v0.4.0` betas carry the newest work. What works today:

- A Controller you run yourself, with Workers on macOS and Linux joined through a wizard and kept up as launchd or systemd services.
- Tasks that run as isolated worktrees on a Worker, with checkpoints, a journal per Worker and backups you can restore on another host.
- Handing a Claude Code or Codex session over to a remote Run (`handto`).
- Attaching to a live Run, and a shell in its worktree, from your terminal, through the Controller.
- A CLI and a TUI; signed releases and a verifying installer.

Next: seeing remote Runs from Claude Code ([#180](https://github.com/Autarquico/codeflock/issues/180)).

## Install

```bash
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.7/install.sh | CFLK_VERSION=v0.4.0-beta.7 sh
```

`releases/latest` skips prereleases: while 0.4.0-beta.7 is the newest release of `cflk`, `…/releases/latest/download/install.sh` installs an older one that still calls the command `cf`. Pin the version, as above, until a stable release has `cflk`.

Then follow the [getting started guide](docs/guide/getting-started.md). Coming from the beta.6 or earlier (the command was `cf`)? Read [Upgrade to 0.4.0-beta.7](docs/guide/upgrade-beta7.md): its installer is `https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.7/install.sh`.

## Documentation

- Guide: [`docs/guide/`](docs/guide/)
- Specification: [`docs/codefabric-spec.md`](docs/codefabric-spec.md)
- Roadmap: §43 of the spec (Phases 0–10, v0.1 → v1.0)
- Architecture decisions: [`docs/adr/`](docs/adr/)

## Build

Requires Go (see `go.mod`) and, for integration tests, `git` and `tmux`.

```bash
make build          # bin/cflk, static, no cgo
make test           # unit tests
make lint           # golangci-lint v2, including layering rules
make cross          # linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
```

```bash
./bin/cflk version
```

## License

[Apache-2.0](LICENSE)
