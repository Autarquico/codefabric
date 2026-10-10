# ADR 0002 — Implementation decisions taken before Phase 0

- Status: accepted
- Date: 2026-10-10

Decisions the spec left open or that refine it, agreed when planning the full implementation.

| # | Topic | Decision |
|---|-------|----------|
| D1 | Resolving `source.ref` to a commit | The **client** resolves it with `git ls-remote` using its own credentials (`cf mcp serve` also runs on the client host). The Controller never asks for origin credentials and refuses the URL forms that carry them; a secret typed as a URL user name or path is not detected (spec B5). |
| D2 | PKI algorithms | The Controller's identity **is its CA**, so there is no separate ed25519 keypair (nothing would use it). The CA, the server certificate and the client certificates are all **ECDSA P-256**, for compatibility with git/libcurl TLS stacks; the Controller rejects any other key type in a CSR. Validated by `TestGitTrustsControllerCert` (build tag `integration`), which runs `git ls-remote` against a server using the issued certificates on macOS and Ubuntu. Client keys are generated on the client and never leave it; the Controller builds every certificate from its own template (CN = identity ID, client-auth only, no SANs or extensions copied from the CSR). |
| D3 | Who runs `promote` | A **Worker holding the origin secret**, through a durable command. Compatible with "approving executes" (§16.3). |
| D4 | Provider admin keys | Stored in a `SecretStore` local to the Controller host, separate from Run secrets (§16.7 governs Run secrets). |
| D5 | CA key in backups | Included **only encrypted with `--passphrase`** (age). Without it, restoring requires Workers and clients to re-join. |
| D6 | Spec location | `docs/codefabric-spec.md`. |
| D7 | Headless permissions | Headless Runs use pre-granted permissions (Claude `--permission-mode bypassPermissions --disallowedTools AskUserQuestion`, Codex approvals `never`). On Workers without isolation (tmux runtime) `cf doctor` warns; a dedicated OS user per Worker is recommended. Real isolation arrives with the docker runtime (Phase 10). |
| D8 | Config and data paths | `$XDG_CONFIG_HOME/codefabric` (default `~/.config/codefabric`) and `$XDG_DATA_HOME/codefabric` (default `~/.local/share/codefabric`) on **every** OS, macOS included, instead of `~/Library/Application Support`. A CLI user and a script find the same predictable paths everywhere. `CF_CONFIG_DIR` and `CF_DATA_DIR` override them (tests, deployments); `cf config paths` prints the effective ones. Client credentials are a `0600` file bundle in `<config>/credentials/` rather than the OS keychain, which §16.6 allows: no keyring dependency, and the same code path on macOS and Linux. |

## Technical stack

Go, pure-Go SQLite (`modernc.org/sqlite`, no cgo) in WAL mode with `synchronous=FULL`, goose migrations (embedded SQL), `oklog/ulid`, stdlib TLS, shell-out to `git`/`tmux`/`ssh`, `net/http` stdlib with hand-written SSE, Cobra, Bubble Tea v2, the official MCP Go SDK, `log/slog`, golangci-lint v2, GoReleaser v2.
