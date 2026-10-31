# Harness `claude-code`

The adapter of Claude Code (Anthropic). A Run gets it when it asks for
`requirements.harness: claude-code`. This file documents it as §7.3 of
`docs/codefabric-spec.md` asks every adapter to. Verified against
`2.1.314 (Claude Code)`.

Registered in the binaries: `cmd/cf` and the Worker daemon
(`internal/worker/daemon/run.go`) offer it next to `codex`.

1. **Installation.** `Detect` looks for `claude` in the PATH of the host
   (`Host.LookPath`) and reports its path; not finding it is not installed,
   not an error. It says nothing of authentication, which `Diagnose` reports
   apart: the secret every Run needs and whether `~/.claude/.credentials.json`
   exists. `Version` runs `claude --version` and parses `X.Y.Z (Claude Code)`
   to `X.Y.Z`. `Install` is unsupported and not declared, `DryRun` included, as in
   `codex`: the user installs Claude Code.

2. **Configuration paths detected.** `~/.claude/`: `settings.json`,
   `CLAUDE.md`, `commands/`, `agents/`, `skills/` and `output-styles/`; and
   from `~/.claude.json` only the `mcpServers` field. Never
   `.credentials.json`, `projects/`, `todos/`, `statsig/`, the history, the
   sessions, nor any other field of `~/.claude.json` (the account, the trust
   of each project, the counters). It declares `ConfigIsolation`: the
   configuration of a Run is materialised in its run directory, which Claude
   Code reads through `CLAUDE_CONFIG_DIR` (the `ConfigDir` of the Run, used as
   it is).

3. **Adoptable components.** `settings.json`, `CLAUDE.md`, `commands`,
   `agents`, `skills`, `output-styles` and `mcpServers`, those that exist, all
   managed. `settings.json` and `mcpServers` are read as JSON and written
   again from what was parsed: keys sorted, two spaces, numbers as written.
   The unit of `mcpServers` is the file `.claude.json` with that one field.
   A directory with anything but regular files (a symlink) is an error.

   Secrets, a closed policy: every string value of an `env` object (in
   `settings.json` and in an MCP server) and of a `headers` object (in an MCP
   server and in an HTTP hook) becomes a `{{cf-secret:<ref>}}` marker, whether
   or not it looks like a secret; empty values stay. The ref carries the file
   and the path: `claude/settings/env/<VAR>`,
   `claude/settings/hooks/<event>/<i>/hooks/<j>/headers/<name>`,
   `claude/claude.json/mcpServers/<server>/env/<VAR>`. A segment with
   characters a ref cannot have is sanitised and told apart by a hash. A
   value that already is a marker stays: it is a ref the person wrote. In a
   Run the markers are rendered as JSON strings (`json-string` escape) once
   the secrets are granted, and removed at its release. A marker is only valid
   inside a JSON string value, never in a key, and only in those two files:
   anything else is refused. Applied globally, nothing with a marker is
   written (`CF_SECRET_UNAVAILABLE`); `settings.json`, the files and the
   directories of managed components are replaced, local ones never, and the
   `mcpServers` of `~/.claude.json` are merged server by server, keeping every
   other field and server of the person.

   Known limit, as the `args` of Codex: the rest is copied as written. The
   commands of `apiKeyHelper`, `otelHeadersHelper` and `headersHelper`, the
   `args` and the URLs of MCP servers, the `command` of hooks and whatever the
   person writes in `CLAUDE.md`, commands, agents and skills may hold a
   credential, and nothing marks or removes it. Secrets go through `env` and
   `headers`, never in those. `.claude.json` is rewritten whole by a global
   apply (atomically, other fields as they were): a Claude Code that writes it
   at that moment may lose the race.

   Excluded: API keys (`ANTHROPIC_API_KEY`, `apiKeyHelper`) and external
   providers (Bedrock, Vertex, Foundry) are not a way to authenticate a Run
   here. They are not blocked: an `env` in `settings.json` that sets them goes
   through the secrets above, but nothing selects or checks them.

4. **Auth.** Every Run declares the secret `claude/oauth-token`, delivered as
   the variable `CLAUDE_CODE_OAUTH_TOKEN` (`RequiredSecrets`, a
   `harness.SecretRequirer`; the token comes from `claude setup-token`). It
   goes through the normal authorisation of the Controller (allowlist of the
   Project, policy of the Worker), and a Worker without it is not eligible
   (§16.7). It is the same on macOS and Linux. Besides, when
   `~/.claude/.credentials.json` exists and `LinkAuth` is asked, the Run
   configuration gets a symlink `.credentials.json` to it (`ApplyResult.AuthLinks`,
   §8.12): a link and not a copy, so the file the Worker keeps is the one that
   is refreshed. Claude Code on macOS keeps its login in the Keychain, which
   does not follow `CLAUDE_CONFIG_DIR`; there the token is what authenticates.
   The adapter is not an `AuthSyncer`: it keeps no copies.
   Adopting the login as a managed secret is a decision of the user: nothing
   here reads `.credentials.json`.

5. **Start and resume.** `Start` builds, without writing anything, the
   process of the Run, named by its path in the host. Headless:
   `claude -p --output-format stream-json --verbose --permission-mode
   bypassPermissions --disallowedTools AskUserQuestion [--model M] -- <prompt>`,
   with `StructuredStdout` (stdout to `events.jsonl`, stderr to the output).
   Interactive: `claude --permission-mode bypassPermissions [--model M] --
   <prompt>`. Both with `CLAUDE_CONFIG_DIR=<ConfigDir>` and
   `Launch.SecretEnv = RequiredSecrets()`. Nobody answers a permission prompt,
   hence `bypassPermissions`: a headless Run never blocks on one (§43).

   `bypassPermissions` lets the agent use every tool without asking,
   including Bash, file edits and network access, as the Worker user. The
   host provider adds no isolation (ADR 0002 D7), so the Run is only as
   contained as that user. Among other things the agent can read
   `CLAUDE_CODE_OAUTH_TOKEN` from its own environment, and so can any command
   it runs (`env`, a script, a child process), and nothing stops it from
   sending the token out. The codex adapter runs with `--sandbox
   workspace-write`, which limits what the agent can write; `bypassPermissions`
   has no such limit, so a compromised Run is only as contained as the Worker
   user. The token should be one whose loss is acceptable (`claude
   setup-token`, revocable) and Runs should
   only go to Workers whose user is trusted with it. `AskUserQuestion` is disabled
   (`--disallowedTools`): when the agent needs input the Run ends as
   `input_required`. It refuses an empty prompt, an empty ConfigDir, a
   model that starts with `-` and a mode that is not declared. `Resume`,
   `SendInput`, `Stop` and `Install` are unsupported: the only stop is the
   signal to the process group.

6. **Event format.** The stream-json of `claude -p`, one JSON object per line;
   what is not JSON or not understood is skipped. Each line is mapped on its
   own, with no state between lines (the executor reads chunks of whole
   lines). `system/init` gives `session_started` with the session id and the
   model. `result` gives the usage events (point 7) and then `result`;
   `is_error` or a subtype starting with `error` gives `error` and a failed
   `result` (a Claude Code without a login answers `is_error: true` with
   subtype `success`). `input_required` is the attempt to ask the person: a
   `tool_use` of `AskUserQuestion` in an `assistant` message, or a
   `system/permission_denied` of that tool. The two reports of one attempt are
   two events. Other denials, and `permission_denials` of the `result`, do
   not count. The exit code of the process is still what ends the Run.

7. **Model and usage.** `ModelSelection` passes `--model`; `UsageEvents` is
   declared. Usage has a single source, the `result` (it already adds up the
   assistant messages, which are not counted): one event per entry of
   `modelUsage` (by model name), or, with none, one event with no model from
   `usage`: `input_tokens`, `output_tokens`, `cache_read_input_tokens`,
   `cache_creation_input_tokens`. A field not reported is unknown, never 0.
   `CollectUsage` reads the same from `events.jsonl`. Cost is not taken from
   `total_cost_usd`. The shape of a successful stream is built from the
   documented schema (`testdata/success-handbuilt.jsonl`); only the failed one
   (`notlogged-2.1.314.jsonl`) is a real recording of 2.1.314.

8. **Checkpoint and handoff.** `Checkpoint` is unsupported: a handoff carries
   what the Run left in the worktree. `Stop` is unsupported: the only stop is
   the signal to the process group.

A Run configuration also gets a `.claude.json` that no snapshot has: the
`mcpServers` of the snapshot and `hasCompletedOnboarding: true`, so that
Claude Code does not stop at its first-run questions. That field is made up
for the Run; a global apply never writes it.
