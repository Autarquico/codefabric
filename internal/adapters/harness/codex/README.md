# Harness `codex`

The adapter of the Codex CLI (OpenAI). A Run gets it when it asks for
`requirements.harness: codex`. This file documents it as §7.3 of
`docs/codefabric-spec.md` asks every adapter to. Verified against
`codex-cli 0.160.1`.

1. **Installation.** `Detect` looks for `codex` in the PATH of the host
   (`Host.LookPath`) and reports its path; not finding it is not installed,
   not an error. `Version` runs `codex --version` and parses
   `codex-cli X.Y.Z` to `X.Y.Z`. `Install` runs `npm install -g @openai/codex` if `npm`
   is in the PATH, else on macOS `brew install codex` if `brew` is, else it
   fails with the manual instructions; `DryRun` only returns the plan. The
   binary may not be in the PATH of the process until a new shell: run
   `Detect` again.

2. **Configuration paths detected.** `~/.codex/`: `config.toml`,
   `AGENTS.md`, `prompts/` and `skills/`. Never `auth.json`, the sessions or
   the history. It declares `ConfigIsolation`: the configuration of a Run is
   materialised in its run directory and Codex reads it through `CODEX_HOME`.

3. **Adoptable components.** `config.toml`, `AGENTS.md`, `prompts/` and
   `skills/`, those that exist. `config.toml` is read as TOML and written
   again from what was parsed: the output is deterministic and loses the
   comments. It loses the keys of the machine: `projects` (its trust),
   `sandbox_workspace_write` and `permissions` (its sandbox and permission
   profiles; the sandbox of a Run is the one `Start` sets). The value of
   every secret becomes a `{{cf-secret:codex/<path>}}` marker: `env`,
   `http_headers` and `query_params` of MCP servers and model providers,
   `experimental_bearer_token`, `shell_environment_policy.set`, the `headers`
   of the OTLP exporters of `otel` and of the MITM hooks of `permissions`. The
   names of variables (`env_vars`, `env_http_headers`, `env_key`,
   `bearer_token_env_var`, the `env` of a credential provider) stay as they
   are. Known limit: the `args` of an MCP server are copied as written
   (see below). In a Run
   the markers are rendered as TOML basic strings once the secrets are
   granted, and removed at its release. Applied globally,
   `config.toml` is merged key by key, keeping `projects`, `sandbox_workspace_write`, `permissions` and every
   local key;
   managed components are written and removed, local ones never; a snapshot
   with secrets is refused there (`CF_SECRET_UNAVAILABLE`).

   Decision: the `args` of an MCP server or a command are kept as they
   are, in every snapshot and Run. A list of arguments cannot be told apart
   into secret and not, and a secret in `args` is neither marked nor
   redacted by name. Secrets go through `env`, `http_headers` or
   `bearer_token_env_var`, which are secret-bearing; never in `args`.

4. **Auth.** The login of the user lives in `auth.json`, which is never part
   of a snapshot. Each Run gets its own copy, mode 0600, in its `CODEX_HOME`
   (`ApplyResult.AuthCopies`), and the daemon (`internal/worker/authsync`)
   propagates the most recent valid one, by `last_refresh`, to the host and to
   the copies of the live Runs, always by atomic rename. Codex rewrites
   `auth.json` in place with truncate and write and no lock
   (`codex-rs/login/src/auth/storage.rs`, 0.160.1), so a link shared by two
   Runs that refresh at once could be left half written; this departs from
   §8.12, which links the auth. `Launch` carries no `SecretEnv` of its own.

5. **Start and resume.** `Start` writes nothing and is idempotent. It refuses
   an empty prompt, an empty `ConfigDir` (Codex would read the global one) and
   a mode outside `headless` and `interactive` (`ErrUnsupportedMode`). Both
   modes add `CODEX_HOME=<ConfigDir>`, run the sandbox `workspace-write`
   without approvals, set the root with `-C <Worktree>`, add `-m <Model>` when
   the Run names one, and pass the prompt after `--` so a prompt that starts
   with `-` is never a flag. A model that starts with `-` is refused: it
   would be read as an option.
   - headless: `codex exec --json --sandbox workspace-write -c
     approval_policy="never" --skip-git-repo-check -C <Worktree> [-m <Model>]
     -- <Prompt>`, with `StructuredStdout`. `codex exec` reads stdin even with
     the prompt in argv; the wrapper gives a headless Run `/dev/null`.
   - interactive: `codex --sandbox workspace-write -a never -C <Worktree>
     [-m <Model>] -- <Prompt>`.
   `Resume` is unsupported.

   Checked in `codex --help` (0.160.1): `-a, --ask-for-approval` exists on the
   interactive command with `never` as a value, and `-s/--sandbox` accepts
   `workspace-write`. `codex exec --help` has no `-a`, hence
   `-c approval_policy="never"` in headless. `-a never -s workspace-write -C /tmp
   --version` is accepted. `--` rests on the argument parser (clap) and was
   not run against a live model.

6. **Event format.** `codex exec --json` writes JSONL to stdout, which the
   wrapper keeps apart in `events.jsonl` (`StructuredStdout`), redacted line
   by line. `Events` maps `thread.started` to `session_started` (the thread ID
   is the session), `turn.completed` to `usage` (when it carries one) and
   `result` (its summary is the last `agent_message`), `turn.failed` to
   `error` and a failed `result`, and a top-level `error` to `error`.
   Warnings (`item.completed` of type `error`), other items, and lines that are
   not JSON, are cut or of an unknown type are skipped. `SendInput` and the
   input signal are unsupported.

7. **Model and usage.** `ModelSelection` and `UsageEvents` are declared; the
   model goes with `-m`. No event carries the model, so it stays empty.
   `CollectUsage` reads `events.jsonl` and gives one usage per
   `turn.completed`: `InputTokens` is `input_tokens` without
   `cached_input_tokens` (Codex counts the cache inside the input, §14.4 does
   not), `CacheReadTokens` is `cached_input_tokens`, `CacheWriteTokens` is
   `cache_write_input_tokens` and `OutputTokens` is `output_tokens`, which
   already holds `reasoning_output_tokens`. What Codex does not report stays
   unknown, never zero; a Run without usage reports none.

8. **Checkpoint and handoff.** `Checkpoint` is unsupported: a handoff carries
   what the Run left in the worktree. `Stop` is unsupported: the only stop is
   the signal to the process group.
