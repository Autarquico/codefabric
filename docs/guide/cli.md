---
title: CLI reference
description: Every top-level cf command, with its most useful subcommands and flags.
---

This page is written by hand from `cf --help`. For the full list of flags of any command, run `cf <command> --help`; that output is the authority.

Flags that every command accepts:

| Flag | Meaning |
| --- | --- |
| `--json` | Output as JSON. Errors too, as `{"error":{code,message,details}}` on stdout. |
| `--controller host:port` | Use this Controller for this run, instead of the one stored when you connected. |
| `--debug` | Show the details of an error. |

Run `cf` with no arguments in a terminal to open [the TUI](../tui/). Otherwise it prints help.

## `cf attach`

Open the live session of a Run in this terminal, over ssh: `cf attach <task|run>`. Closing it does not end the Run; detach with the runtime's own keys (in tmux, Ctrl-b d).

- `--read-only`: attach without sending what you type.
- `--ssh-target [user@]host[:port]`: reach the Worker there instead of where it announced itself.
- `--project`: narrow a Task name.

## `cf client`

Manage client machines. Subcommands: `list`, `remove` (revoke a client) and `renew` (replace this machine's certificate before it expires).

## `cf completion`

Generate the autocompletion script for a shell (`bash`, `zsh`, `fish`, `powershell`).

## `cf config`

Inspect CodeFabric's configuration. `cf config paths` prints where it keeps its files; `cf config show` prints the effective configuration with secrets redacted.

## `cf connect`

Enrol this machine with a Controller using an invitation: `cf connect [invitation | -]`. With `-`, or with no argument, the invitation is read from stdin or asked for, which keeps it out of shell history.

- `--name`: name for this machine (default: hostname).
- `--force`: replace the credentials already stored.

## `cf controller`

Run and administer the Controller.

- `init`: create the database, CA, certificates and owner credentials. Flags: `--advertise host:port`, `--listen`, `--name`.
- `run`: serve the API until interrupted.
- `token create (--client | --worker)`: create a single-use invitation; `--ttl` sets how long it lasts (default 1h, at most 168h).
- `install-service` and `uninstall-service`: run the Controller as a systemd user service (Linux).

## `cf do`

`cf do "<prompt>"` creates a Task from this directory's repository and starts it. It is a shortcut of `cf task new` and takes the same flags:

- `--harness`: `claude-code`, `codex` or `command`.
- `--model`: the model to use.
- `--project`: the Project (default: the one whose repository is this directory's origin).
- `--ref`: branch or tag to start from.
- `--name`: name of the Task (default: made from the prompt).
- `-i, --interactive`: run it interactively instead of headless.
- `--permissions ask`: have the harness ask what it is not allowed to do, answered with `cf task respond`.
- `--completion`: `first_success`, `promote` or `manual`.
- `--no-start`: create the Task without a Run.
- `--secret-env NAME=ref`: give the harness a secret held by the Worker, in variable NAME. Repeatable.
- `--request-id`: idempotency key; repeating the command with it creates nothing twice.

## `cf doctor`

Check that the Controller answers, list every Worker with the verdict of its own doctor per component, and try a non-interactive ssh to each Worker (which `cf attach` needs). An unreachable Controller fails the command; the rest are warnings. For one machine's local detail, use `cf worker doctor`.

## `cf environment`

Apply and inspect Environments. Subcommands: `apply` (make the definition in a file the next version), `list`, `show`.

## `cf events`

Show the events of the Controller (the last 20 by default) and follow them with `-f, --follow`. Filters: `--project`, `--task`, `--run`, `--worker`, `--type` (repeatable), `--level`, `--since` (`2h` or an RFC 3339 time). `--after` resumes from a cursor, and `--limit` sizes the page. With `--json --follow` the output is JSON Lines.

## `cf inbox`

List what needs you: Tasks waiting for input and failures to decide on. Answer with `cf task respond`.

## `cf logs`

Two different things, never mixed:

- The structured logs of the Controller and the Workers. This is the default, and what any of `--worker`, `--project`, `--task`, `--level`, `--since`, `--after`, `--limit` or `--structured` gives.
- The output of one Run, byte for byte: `cf logs --run <run>`, with `--tail` and `-f, --follow`. Add `--structured` to see that Run's structured logs instead.

Escape sequences and control characters are removed so a Run cannot act on your terminal; `--raw` passes them through.

## `cf mcp`

Use CodeFabric from Claude Code or Codex. See [Use cf from Claude Code or Codex](../mcp/).

- `cf mcp serve [--profile read|execute|modify]`: the MCP server, over stdio. Default profile: `execute`. Standard output carries only the protocol; `--json` is rejected. The tool starts it; you rarely run it.
- `cf mcp install --claude|--codex [--scope user|project] [--profile …] [--dry-run]`: register the server and write the `codefabric` skill. Idempotent.
- `cf mcp uninstall --claude|--codex [--scope user|project]`: remove both. With `--codex --scope project` only the repository's skill goes: the Codex server is user-wide and stays registered (`cf mcp uninstall --codex` removes it).

## `cf notify`

Desktop notifications and hooks for the events that need you. `cf notify watch` notifies as events happen, until interrupted; `cf notify test` sends a sample through the native channel and the hook.

## `cf project`

Manage Projects. Subcommands: `create` (the repository defaults to this directory's origin), `list`, `show`, `set` (change defaults; only the flags given change), `explain` (the effective configuration at a commit, and where each field comes from) and `secrets` (the secrets the Runs of a Project may use).

## `cf ps`

List the Runs that are live, with their Workers and whether they can be attached. `--project` narrows it.

## `cf run`

Inspect and explain Runs. Subcommands: `list`, `show` (a Run and the configuration it froze), `explain` (why a Run waits, Worker by Worker) and `publish` (try again to publish the result of a Run whose publication failed). `fork` and `promote` exist but are not implemented yet.

## `cf search`

`cf search <query>` resolves the query as a reference first (short ID, ULID prefix, name), listing every candidate when it is ambiguous, then lists the entities whose text matches. `--kind` restricts it to `project`, `task`, `run`, `worker` or `workflow`; `--project` and `--limit` narrow it.

## `cf setup`

Set up this machine step by step: ask what it is for and chain `cf connect` (client), `cf worker join` (Worker) or both. `--role client|worker|both` skips the question. It does not set up the Controller; use `cf controller init`.

## `cf status`

Show the Controller's version, uptime and database health.

## `cf task`

Create and manage Tasks.

- `new`: create a Task and, unless `--no-start`, its first Run (same flags as `cf do`).
- `list`: list Tasks. `--project`, `--state`, and `--all` to include archived ones.
- `show`: a Task and its Runs. `timeline`: its events, oldest first.
- `respond <task>`: answer what a Run waits on. `--allow` or `--deny` for a permission, `--text` for a question; `--prompt` and `--run` choose among several.
- `set`: rename a Task or change its completion or retry policy.
- `cancel`, `complete` (mark it completed by hand), `archive` (frees its name).
- `defer`, `move` and `resume` exist but are not implemented yet.

## `cf version`

Print the `cf` version.

## `cf worker`

Run and administer Workers.

- `join <invitation | ->`: enrol this machine as a Worker, with a wizard in a terminal. Flags include `--name`, `--max-concurrent`, `--cpu-max`, `--memory-max`, `--disk-max`, `--availability always|idle|manual`, `--resume`, `--non-interactive` and `--yes`.
- `run`: run the Worker daemon until interrupted.
- `doctor`: check this machine as a Worker: tmux, git, disk, harnesses and their logins.
- `list`, `show`: Workers with their resources and version.
- `drain`, `undrain`: stop a Worker from taking new Runs, or let it take them again.
- `remove`: remove a Worker and revoke its credentials (only without active Runs).
- `secrets`: manage the secrets this Worker holds for Runs.
- `install-service`, `uninstall-service`: run the Worker as a service of this user (systemd or launchd).

## `cf workflow`

Adopt, version and inspect Workflows. Subcommands: `adopt` (create one from this machine's native configuration), `commit` (a new version), `list`, `show`, `history`, `diff` and `explain` (dry runs), `apply` (to a Worker), `sync` (to every online Worker), `status` (what each Worker holds, and drift) and `rollback`.
