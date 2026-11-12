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

## `cf claude`

Install into Claude Code what cf needs there: the `/handto` skill, which hands the session you are in over to a Worker (`/handto <worker>`). The skill is embedded in the binary and carries the version of the `cf` that wrote it.

- `cf claude install [--scope user|project] [--force] [--dry-run]`: write it to `~/.claude/skills/handto/SKILL.md` (`--scope project`: `.claude/skills/` of the repository). Idempotent: the same version changes nothing and another version updates it. A `handto` skill that cf did not write is not overwritten without `--force`.
- `cf claude uninstall [--scope user|project]`: remove the skill that cf wrote. One it did not write stays.
- `cf claude status [--scope user|project]`: `missing`, `current`, `outdated` (another version of cf), `modified` (cf wrote it, then it was edited; `install` restores it) or `foreign`. With `--json`, the same as an object.

`cf doctor` warns when the user skill is missing, of another version or edited since cf wrote it, and `cf setup` offers to install it to a client on a terminal. The skill runs `cf handoff` (see below).

## `cf codex`

The same for Codex: `cf codex install|uninstall|status [--scope user|project]` manages the `handto` skill in `~/.agents/skills/handto/SKILL.md` (`--scope project`: `.agents/skills/` of the repository), with the same rules as `cf claude` (versioned, idempotent, a foreign `handto` is not overwritten without `--force`). Codex has no slash commands with arguments: you ask for it by name (`$handto omarch02`) and the skill runs `cf handoff --to <worker>`, which reads the session from `CODEX_SESSION_ID`. `cf doctor` warns when the skill is missing and `codex` is on the `PATH`.

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
- `--snapshot`: start from this working tree as it is, uncommitted changes and untracked (not ignored) files included. The tree is committed on a temporary index and sent to the Controller as a snapshot, so `git status` is the same before and after; the Run starts from that commit and records the branch you were on. It cannot go with `--ref` or `--no-start`.
- `--secret-env NAME=ref`: give the harness a secret held by the Worker, in variable NAME. Repeatable.
- `--request-id`: idempotency key; repeating the command with it creates nothing twice.

## `cf doctor`

Check that the Controller answers, list every Worker with the verdict of its own doctor per component, and try a non-interactive ssh to each Worker (which `cf attach` needs). An unreachable Controller fails the command; the rest are warnings. For one machine's local detail, use `cf worker doctor`.

## `cf environment`

Apply and inspect Environments. Subcommands: `apply` (make the definition in a file the next version), `list`, `show`.

## `cf events`

Show the events of the Controller (the last 20 by default) and follow them with `-f, --follow`. Filters: `--project`, `--task`, `--run`, `--worker`, `--type` (repeatable), `--level`, `--since` (`2h` or an RFC 3339 time). `--after` resumes from a cursor, and `--limit` sizes the page. With `--json --follow` the output is JSON Lines.

## `cf gc`

Collect garbage in the Controller: history past its retention (events, logs, delivered orders, the output of finished Runs), the blobs nothing refers to, and the branches and orphan refs that the git relay keeps for archived Tasks. `--dry-run` shows counts (and bytes, where it can measure them) by kind of what a pass would delete, and deletes nothing. It deletes in small batches, so it does not stall the Controller; the Controller also runs it by itself, hourly. A pass started by a Client leaves an entry in the audit trail (who, how it ended, how much it deleted), also when it fails; a dry run leaves none. It does not touch Workers: each cleans its own machine.

Retention is set in `controller.json`, as text such as `"30d"` or `"45s"` (at least 1s). What you leave out takes the default:

| Key | Default | Keeps |
| --- | --- | --- |
| `retention.system` | `30d` | logs that belong to no Run |
| `retention.events` | `180d` | events (those of a Run that has not ended stay) |
| `retention.run_logs` | `30d` | logs of a Run, and orders already delivered to its Worker (those of a Run that is still alive stay) |
| `retention.raw_output` | `7d` | the output of a finished Run |
| `retention.relay_branches` | `30d` | branches and orphan refs of an archived Task, counted from its archiving |
| `gc.batch_size` | `500` | most rows one transaction touches |
| `gc.interval` | `1h` | time between two automatic passes |
| `gc.blob_grace` | `24h` | how long a blob nothing refers to is left alone |
| `max_storage` | none | a number of **bytes**; only measured, nothing is deleted for it |

A Worker keeps the worktree of a published Run for `retention.worktrees` in its own `worker/config.json` (default `7d`).

## `cf handoff`

What `/handto` runs: `cf handoff --to <worker>` hands the session that calls it over to a Worker. Inside a Run on a Worker (`CF_RUN_SOCKET` is set, for interactive Runs) it asks the Worker, over a local socket, to move the Task of the Run to that Worker: no client credentials are involved, the Controller grants it only while the Run is live under its lease, and it is audited as asked from the Run. `--list` prints the Workers by name and state. `--session <id>` is for the client side, outside a Run.

From the machine of the user (no `CF_RUN_SOCKET`): `cf handoff --session <id> --to <worker>` (Codex: no `--session`, it reads `CODEX_SESSION_ID`) exports the session of Claude Code or Codex, cut after its last tool call (the call to `cf handoff` itself; a session open elsewhere is refused), snapshots the repository (uncommitted and untracked files travel, the working tree is untouched, commits not pushed travel too) and creates an interactive Task on that Worker with the session as its seed. If the session already has a Task (the local registry `session -> task`), it resumes that Task on the Worker instead. It prints the Task, the Worker and `cf attach <task>`, and leaves the local `handed-off` marker. The Worker must be online (otherwise `CF_NOT_FOUND` lists the Workers) and the Project must keep native state.

- A second handoff of a session with the marker is refused (two writers on the same work): `--force` resumes the Task on the other Worker.
- A session that holds what looks like a secret is blocked, with the lines: `--allow-sensitive-session` sends it anyway.
- `--force` and `--allow-sensitive-session` are accepted only when stdin is a terminal. Claude Code runs Bash without one, so the skill cannot lift these guards: the error prints the exact command to run in your own terminal.

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

`cf run checkpoint` captures and lists checkpoints (the Run's source pushed to a ref of the relay, plus the harness's own state when the harness supports it):

- `create <run>`: ask the Worker of a live Run for a manual checkpoint. The Run is `checkpointing` while it captures. `--request-id` makes repeating the command ask once. It fails with `CF_CHECKPOINT_UNSUPPORTED` if the Worker cannot capture.
- `list <run>` or `list --task <task> [--project <p>]`: the published checkpoints, oldest first, with their trigger, whether they carry native state, the paths the capture left out and the warnings of the capture, such as a secret found in the session (both listed under the table). `--json` prints the same, with the `summary` of each checkpoint.
- `show <run> <checkpoint>`: the summary of one checkpoint, by ID or number. The Controller composes it when the checkpoint is published, without a model: commits since the Run's base with their files, the Task prompt, what the capture left out and whether there is native state, and the latest events. Warnings of the capture are printed after the summary. Each part is bounded and says what it cut. It is what a Run that resumes without native state starts from.

## `cf search`

`cf search <query>` resolves the query as a reference first (short ID, ULID prefix, name), listing every candidate when it is ambiguous, then lists the entities whose text matches. `--kind` restricts it to `project`, `task`, `run`, `worker` or `workflow`; `--project` and `--limit` narrow it.

## `cf setup`

Set up this machine step by step: ask what it is for and chain `cf connect` (client), `cf worker join` (Worker) or both. `--role client|worker|both` skips the question. It does not set up the Controller; use `cf controller init`.

## `cf status`

Show the Controller's version, uptime and database health.

## `cf storage`

Inspect what the Controller stores. `cf storage stats` shows the bytes by category (output and Workflow blobs, blobs nothing refers to, database with the part that is logs, relay) and what a `cf gc` pass would free: bytes for blobs and logs, counts for the rest. Purged rows of the database are reused, so its file does not shrink. It changes nothing. `max_storage` only shows here and as a warning in the Controller's log; nothing is deleted for it.

## `cf task`

Create and manage Tasks.

- `new`: create a Task and, unless `--no-start`, its first Run (same flags as `cf do`).
- `list`: list Tasks. `--project`, `--state`, and `--all` to include archived ones.
- `show`: a Task and its Runs. `timeline`: its events, oldest first.
- `respond <task>`: answer what a Run waits on. `--allow` or `--deny` for a permission, `--text` for a question; `--prompt` and `--run` choose among several.
- `set`: rename a Task or change its completion or retry policy.
- `cancel`, `complete` (mark it completed by hand), `archive` (frees its name).
- `defer <task>`: suspend the Task's running Run with a checkpoint (the work goes to the Controller's relay, never to `origin`, and the harness's session with it if the Project keeps native state). The Task waits (deferred) once the Worker has published the capture. Deferring a deferred Task changes nothing; a Task with no running Run, or busy with a capture for another reason, is refused (exit 4). `--request-id` makes repeating the call safe.
- `resume <task> [--worker <name>]`: queue a new Run that continues the deferred Task from the last checkpoint of the deferred Run, which is never started again. `--worker` offers the Run to that Worker only, which prepares its source from the relay. If the harness's session cannot be restored there (another adapter, or another major version than the Worker announces) the Run continues from the source and the summary alone, and the `RunAssigned` event carries the warning. A Task that is not deferred is refused (exit 4).
- `move <task> [<worker>] [--worker <name>] [--force]`: continue the Task on another Worker. It checkpoints the Run (trigger `move`), waits for it to end and then queues a new Run on the target, pinned to `--worker` (or wherever the scheduler puts it). The new Run is only created once the old one is terminal, so the old one cannot report anything afterwards. The move is durable, in stages (`requested`, `checkpointing`, `checkpointed`, `resuming`, `done`) that never go back; the Controller resumes it after a restart, and `cf task show` says where it is. A move that is not over keeps its checkpoint from the GC. If the Run's Worker is offline the Run must be `lost` first: the command refuses (exit 8) and says which checkpoint it would continue from and that what the Run did since is lost; `--force` marks it lost (audited) and goes on. If the target cannot be prepared the stage stays, the Task waits (decision) and repeating `cf task move` retries from that stage without capturing again.

## `cf version`

Print the `cf` version.

## `cf worker`

Run and administer Workers.

- `join <invitation | ->`: enrol this machine as a Worker, with a wizard in a terminal. Flags include `--name`, `--max-concurrent`, `--cpu-max`, `--memory-max`, `--disk-max`, `--availability always|idle|manual`, `--resume`, `--non-interactive` and `--yes`.
- `run`: run the Worker daemon until interrupted.
- `doctor`: check this machine as a Worker: tmux, git, disk, harnesses and their logins.
- `list`, `show`: Workers with their resources and version.
- `drain`, `undrain`: stop a Worker from taking new Runs, or let it take them again. `drain --move` also moves each Task that runs on the Worker (`cf task move`, the scheduler chooses where); Tasks that cannot be moved are listed with why. `drain --force` cancels the Runs the Worker has, after asking each a checkpoint (see [Automatic checkpoints](#automatic-checkpoints)); it asks you to confirm, or takes `--yes`, and cannot be combined with `--move`.
- `remove`: remove a Worker and revoke its credentials (only without active Runs).
- `secrets`: manage the secrets this Worker holds for Runs.
- `install-service`, `uninstall-service`: run the Worker as a service of this user (systemd or launchd).

## `cf workflow`

Adopt, version and inspect Workflows. Subcommands: `adopt` (create one from this machine's native configuration), `commit` (a new version), `list`, `show`, `history`, `diff` and `explain` (dry runs), `apply` (to a Worker), `sync` (to every online Worker), `status` (what each Worker holds, and drift) and `rollback`.

### Native state of checkpoints

A checkpoint carries the harness's own state (its session) only in a Project that opted in: `cf project set <project> --native-state on|off` (off by default; `cf project show` prints it). With it off, checkpoints are source and summary. With it on, a session that holds the value of a secret of the Run is left out and the checkpoint says so with a warning. The Worker's grace for quiescing a harness is `checkpoint.grace` in its `config.json`; the Controller keeps the last `gc.checkpoint_interval_keep` (5) automatic checkpoints of a Run and drops reservations never published after `gc.checkpoint_reservation_ttl` (7d).

### Automatic checkpoints

Two things take a checkpoint without you asking. Both go to a Run only if its Worker can capture checkpoints (`CF_CHECKPOINT_UNSUPPORTED` otherwise: the Run is just skipped).

- **By interval.** `checkpoint.interval` in the Controller's `controller.json`, as text such as `"10m"`, takes a checkpoint (trigger `interval`) of each Run that is going once that long has passed since its last checkpoint, or since it started. It is off when unset or `0`. The Run goes on after the capture; a Run already capturing is not asked again, and one whose capture failed is asked again only after another interval. The Controller keeps the last `gc.checkpoint_interval_keep` of them per Run. There is one interval for the whole Controller, not one per Project.
- **Before a destructive action.** `cf worker drain <worker> --force` drains the Worker and cancels its Runs. First it asks each Run a checkpoint (trigger `pre-destructive`) and waits for them up to `checkpoint.pre_destructive_timeout` (default `20s`, at most `25s`, since a request to the Controller times out at 30s). A Run whose checkpoint cannot be asked, fails or is not published in time is cancelled all the same; the command prints which Runs were checkpointed first and why the others were not, and the `WorkerDraining` event carries the same list under `forced`.
