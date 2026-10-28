# Harness `codex`

The adapter of the Codex CLI (OpenAI). A Run gets it when it asks for
`requirements.harness: codex`. This file documents it as §7.3 of
`docs/codefabric-spec.md` asks every adapter to. Verified against
`codex-cli 0.160.1`. Points marked *(T2)*, *(T3)* and *(T6)* describe what
those tasks of the plan leave and are completed when they land.

1. **Installation.** `Detect` looks for `codex` in the PATH of the host
   (`Host.LookPath`) and reports its path; not finding it is not installed,
   not an error. `Version` runs `codex --version` and parses
   `codex-cli X.Y.Z` to `X.Y.Z`. `Install` is unsupported and not declared:
   the user installs Codex.

2. **Configuration paths detected.** *(T2)* `~/.codex/`: `config.toml`,
   `AGENTS.md`, `prompts/`, `skills/`. `auth.json` is never snapshotted.
   `config.toml` loses `projects.*` (trust per machine) and its secret values
   become `{{cf-secret:codex/...}}` markers. Until T2, `SnapshotConfig`,
   `ApplyConfig`, `DiffConfig` and `ValidateConfig` are unsupported. It declares
   `ConfigIsolation`: Codex reads the directory of the Run through `CODEX_HOME`.

3. **Adoptable components.** *(T2)* `config.toml`, `AGENTS.md`, `prompts/`,
   `skills/`.

4. **Auth.** *(T6)* The login of the user lives in `auth.json`. It is not part
   of the snapshot. Each Run gets its own copy, mode 0600, in its `CODEX_HOME`,
   and the daemon propagates the most recent one (decision of the plan: Codex
   refreshes it by truncate-and-write with no lock, so a shared link can be
   corrupted by two Runs; see `.claude/loop/codex-adapter/t0.md`). `Launch`
   carries no `SecretEnv` of its own.

5. **Start and resume.** `Start` writes nothing and is idempotent. It refuses
   an empty prompt, an empty `ConfigDir` (Codex would read the global one) and
   a mode outside `headless` and `interactive` (`ErrUnsupportedMode`). Both
   modes add `CODEX_HOME=<ConfigDir>`, run the sandbox `workspace-write`
   without approvals, set the root with `-C <Worktree>`, add `-m <Model>` when
   the Run names one, and pass the prompt after `--` so a prompt that starts
   with `-` is never a flag.
   - headless: `codex exec --json --sandbox workspace-write -c
     approval_policy="never" --skip-git-repo-check -C <Worktree> [-m <Model>]
     -- <Prompt>`, with `StructuredStdout`. `codex exec` reads stdin even with
     the prompt in argv, so the wrapper must give it a closed stdin (T4).
   - interactive: `codex --sandbox workspace-write -a never -C <Worktree>
     [-m <Model>] -- <Prompt>`.
   `Resume` is unsupported.

   Checked in `codex --help` (0.160.1): `-a, --ask-for-approval` exists on the
   interactive command with `never` as a value, and `-s/--sandbox` accepts
   `workspace-write`. `codex exec --help` has no `-a`, hence
   `-c approval_policy="never"` in headless. `-a never -s workspace-write -C /tmp
   --version` is accepted. `--` rests on the argument parser (clap) and was
   not run against a live model.

6. **Event format.** *(T3)* `codex exec --json` writes JSONL to stdout:
   `thread.started`, `turn.started`, `item.completed` (`agent_message`, and
   `error` for non-fatal warnings) and `turn.completed` with usage. Until T3,
   `Events` is unsupported. `SendInput` is unsupported.

7. **Model and usage.** `ModelSelection` and `UsageEvents` are declared; the
   model goes with `-m`. *(T3)* `turn.completed` carries the token usage;
   no event carries the model, so it stays empty unless the Run names it. A
   Run without usage reports none, not zero. Until T3, `CollectUsage` is
   unsupported.

8. **Checkpoint and handoff.** `Checkpoint` is unsupported: a handoff carries
   what the Run left in the worktree. `Stop` is unsupported: the only stop is
   the signal to the process group.
