# ADR 0002 — Implementation decisions taken before Phase 0

- Status: accepted
- Date: 2026-10-10

Decisions the spec left open or that refine it, agreed when planning the full implementation.

| # | Topic | Decision |
|---|-------|----------|
| D1 | Resolving `source.ref` to a commit | The **client** resolves it with `git ls-remote` using its own credentials (`cf mcp serve` also runs on the client host). The Controller never holds origin credentials. |
| D2 | PKI algorithms | Controller identity key is ed25519 (§16.6). The CA and X.509 leaf certificates use **ECDSA P-256** for compatibility with git/libcurl TLS stacks. To be validated by a spike in `controller-identity-tls`. |
| D3 | Who runs `promote` | A **Worker holding the origin secret**, through a durable command. Compatible with "approving executes" (§16.3). |
| D4 | Provider admin keys | Stored in a `SecretStore` local to the Controller host, separate from Run secrets (§16.7 governs Run secrets). |
| D5 | CA key in backups | Included **only encrypted with `--passphrase`** (age). Without it, restoring requires Workers and clients to re-join. |
| D6 | Spec location | `docs/codefabric-spec.md`. |
| D7 | Headless permissions | Headless Runs use pre-granted permissions (Claude `--permission-mode bypassPermissions --disallowedTools AskUserQuestion`, Codex approvals `never`). On Workers without isolation (tmux runtime) `cf doctor` warns; a dedicated OS user per Worker is recommended. Real isolation arrives with the docker runtime (Phase 10). |

## Technical stack

Go, pure-Go SQLite (`modernc.org/sqlite`, no cgo) in WAL mode with `synchronous=FULL`, goose migrations (embedded SQL), `oklog/ulid`, stdlib TLS, shell-out to `git`/`tmux`/`ssh`, `net/http` stdlib with hand-written SSE, Cobra, Bubble Tea v2, the official MCP Go SDK, `log/slog`, golangci-lint v2, GoReleaser v2.
