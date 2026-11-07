---
title: Use cf from Claude Code or Codex
description: Register the cf MCP server in Claude Code or Codex, choose a profile, and drive Tasks from the model without leaving the session.
---

`cf mcp serve` is an MCP server that lets Claude Code or Codex use CodeFabric from inside their own session: create a Task, wait for it, read its output and answer it when it needs input. The model gets a fixed set of tools. It has no shell and cannot read secrets.

The server speaks stdio and uses the identity of the client you enrolled with `cf connect`, so it needs a connected machine. You normally do not run it yourself; the tool starts it.

## Install

```sh
cf mcp install --claude
cf mcp install --codex
```

Each command registers the server with the CLI of that tool (`claude mcp add`, `codex mcp add`), so `claude` or `codex` must be on your `PATH`; if it is not, the command fails before writing anything. It also writes the `codefabric` skill, which tells the model when to use CodeFabric and how. The command is idempotent: running it again replaces the registration and leaves a skill that is already current alone.

| Flag | Meaning |
| --- | --- |
| `--claude`, `--codex` | Which tool to set up. |
| `--scope user\|project` | `user` (default): this machine. `project`: this repository. |
| `--profile read\|execute\|modify` | What the model may do. Default: `execute`. |
| `--dry-run` | Print what would run and what would be written, and change nothing. |

What it writes:

| | Claude Code | Codex |
| --- | --- | --- |
| Registration | `claude mcp add --scope <scope> cf -- cf mcp serve --profile <profile>` | `codex mcp add cf -- cf mcp serve --profile <profile>` |
| Skill, `user` scope | `~/.claude/skills/codefabric/SKILL.md` | `~/.agents/skills/codefabric/SKILL.md` |
| Skill, `project` scope | `.claude/skills/codefabric/SKILL.md` in the repository root | `.agents/skills/codefabric/SKILL.md` in the repository root |

Codex has no project scope for MCP servers. With `--scope project` the server is still registered for your user, and only the skill goes to the repository; the command says so. For Codex, the command also sets `tool_timeout_sec = 660` in the `[mcp_servers.cf]` table of `$CODEX_HOME/config.toml` (`~/.codex/config.toml` by default). Codex cuts a tool call at 60 seconds unless told otherwise, and `events_wait` can block for up to 10 minutes. Re-adding the server with `codex mcp add` drops that setting, so run `cf mcp install --codex` again instead of adding by hand.

The skill is embedded in the `cf` binary. After you update `cf`, run `cf mcp install` again to refresh it.

## Uninstall

```sh
cf mcp uninstall --claude
cf mcp uninstall --codex
```

It removes the registration and the `SKILL.md` it wrote, and the `codefabric` directory only if it is then empty. Other skills are not touched. `--scope` selects the same scope as in the install.

## Profiles and tools

A profile is fixed when the server starts. A tool outside the profile is not listed, and calling it fails. On the wire the `.` of a tool name becomes `_`: `tasks.create` is `tasks_create`.

| Profile | Tools |
| --- | --- |
| `read` | `projects_list`, `projects_get`, `tasks_list`, `tasks_get`, `runs_list`, `runs_get`, `runs_logs`, `workers_list`, `workers_get`, `workflows_list`, `workflows_get`, `workflows_diff`, `inbox_list`, `search`, `logs_query`, `events_query`, `events_wait` |
| `execute` (default) | `read`, plus `tasks_create`, `tasks_respond` |
| `modify` | `execute`, plus `tasks_cancel`, `workers_drain`, `workers_undrain` |

`runs_logs` returns the output of a Run. `workflows_diff` is an apply in dry-run. `inbox_list` and `search` are what `cf inbox` and `cf search` show.

## The flow: create, wait, respond

1. The model calls `tasks_create` with the prompt, the project and a `request_id`.
2. It calls `events_wait` for `RunCompleted`, `RunFailed` or `TaskNeedsInput`, filtered to that Task. It blocks until there is a matching event or the timeout, without polling.
3. On `RunCompleted` or `RunFailed`, it reads `runs_get` and `runs_logs`.
4. On `TaskNeedsInput`, it reads `inbox_list` to see what is asked and calls `tasks_respond` with the answer and a `request_id`.

Every `events_wait` answer carries a `cursor`. Passing it as `after_cursor` in the next call means no event is lost between calls. Without `after_cursor` the wait starts from now, so call it right after `tasks_create`. A cursor older than the retention window fails with `CF_CURSOR_EXPIRED`.

To try it, ask the model something like: "Run the tests of this repository on CodeFabric and tell me what fails." A Task created with `--permissions ask` is the way to see the needs-input step; see [Getting started](../getting-started/).

## request_id

Every tool that changes something (`tasks_create`, `tasks_respond`, `tasks_cancel`, `workers_drain`, `workers_undrain`) requires a `request_id`, and a call without one is rejected. A repeated call with the same `request_id` has no second effect and gets the same answer, so a retry after an error or a timeout cannot create a Task twice. The model should use a new `request_id` for each intended action and reuse it only to retry that action.

## Waiting and timeouts

`events_wait` takes a `timeout`: 5 minutes by default, 10 minutes at most; a larger value is a validation error. When it times out it returns no events and a cursor to continue from.

The client decides how long a call may take:

- **Claude Code:** a call that runs for more than 2 minutes continues as a background task and the model is told when it ends. That is expected for long waits.
- **Codex:** the tool timeout is `tool_timeout_sec`. `cf mcp install --codex` sets it to 660 seconds, above the 10-minute maximum. A server registered by hand keeps Codex's default of 60 seconds, and a long wait will fail.

## Limits

- **Profiles are a local restriction on the model, not a security boundary.** They are applied by `cf mcp serve` on your machine. Anyone who can run commands as you can start the server with another profile or just use `cf`. The Controller does not enforce profiles.
- **There is no shell and no secrets access.** No profile has a tool for either.
- **In `execute` the model can answer its own needs-input.** That includes a permission request on a Task it created. Choose `read` if you want the model to watch only, and answer from `cf inbox` yourself.
- **`read` already sees the output of Runs** through `runs_logs`. Do not give it to a model that should not see what your Tasks print.
- **Not available yet:** Plans, fork, compare and promote, budgets, artifacts, models and confirmations. They will be added when they exist in the CLI.

## Audit

Each `cf mcp serve` opens a session with its own ID. What the model changes is recorded with the actor `mcp`: the identity of your client and the ID of that session. In the event stream you can tell an answer given by the model from one you gave with `cf task respond`. The session ID does not grant anything; permissions come from your client's certificate, as for any `cf` command.

## Running the server by hand

```sh
cf mcp serve --profile read
```

`--profile` is `read`, `execute` (default) or `modify`. Standard output carries only the MCP protocol and diagnostics go to standard error, so do not pipe it through anything else. `--json` is rejected.

## What is tested, and what is not

The install and uninstall commands are tested in CI with fake `claude` and `codex` programs. They have not been run against every version of the real tools; the Codex timeout edit was checked against `codex-cli` 0.160.1.
