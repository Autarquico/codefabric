# CodeFabric

CodeFabric (`cf`) is a local-first control plane for running, moving, observing and comparing coding agents (Claude Code, Codex CLI, …) across a dynamic pool of machines.

> *Tasks belong to CodeFabric; Runs happen on Workers.*

You think in **Projects → Tasks → Results**. CodeFabric takes care of **Workers → Workflows → Harnesses → Models → Logs → Scheduling**.

## Status

Early development. The design is complete and reviewed for the first milestones; the implementation is starting.

- Specification: [`docs/codefabric-spec.md`](docs/codefabric-spec.md)
- Roadmap: §43 of the spec (Phases 0–10, v0.1 → v1.0)
- Architecture decisions: [`docs/adr/`](docs/adr/)

## Build

Requires Go (see `go.mod`) and, for integration tests, `git` and `tmux`.

```bash
make build          # bin/cf, static, no cgo
make test           # unit tests
make lint           # golangci-lint v2, including layering rules
make cross          # linux/amd64, linux/arm64, darwin/arm64
```

```bash
./bin/cf version
```

## License

[Apache-2.0](LICENSE)
