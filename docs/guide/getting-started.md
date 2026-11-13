---
title: Getting started
description: Install cf, start a Controller, join a Worker and run your first Task.
---

This page takes you from nothing to a Task running on a Worker. It assumes a Git repository you can push to, and one machine to start with: the Controller, a Worker and the client can all be the same machine.

## Install

```sh
curl -fsSL https://github.com/Autarquico/codefabric/releases/latest/download/install.sh | sh
```

The installer puts `cf` in `~/.local/bin`. Two variables change that:

| Variable | Meaning |
| --- | --- |
| `CF_VERSION` | The version to install, such as `v0.2.0`. Default: the latest release. |
| `CF_INSTALL_DIR` | Where to put `cf`. Default: `~/.local/bin`. |

```sh
curl -fsSL https://github.com/Autarquico/codefabric/releases/latest/download/install.sh | CF_VERSION=v0.2.0 CF_INSTALL_DIR=/usr/local/bin sh
```

Supported platforms: Linux (amd64, arm64) and macOS on Apple silicon (arm64). Anything else is refused before downloading.

Before it installs anything, the script checks an OpenSSH signature on the list of checksums, then the checksum of the archive. It needs OpenSSH 8.2 or later (`ssh-keygen`). If a check fails, nothing is written to the install directory. At the end it tells you whether the directory is on your `PATH` and suggests a next step; it does not run anything else.

:::note[What has not been verified]
The installer and its signing chain are tested in CI against a throwaway key. It has not yet been verified on a clean machine against a published release.
:::

Check it works:

```sh
cf version
```

## Start a Controller

The Controller is the one process that holds the state. Create it once, then run it:

```sh
cf controller init
cf controller run
```

`init` creates the database, a certificate authority, certificates and the credentials of the machine you are on. By default clients dial `<hostname>:7443`; use `--advertise host:port` if other machines reach this one by another name. `run` serves the API until you stop it. To keep it running, `cf controller install-service` sets it up as a systemd user service on Linux.

`cf controller init` also creates the owner credentials and a name for the owner's machine (`--name`). Once the Controller is running, `cf status` shows its version, uptime and database health.

## Add a Worker

A Worker is a machine that runs Tasks. On the Controller's machine, create a single-use invitation:

```sh
cf controller token create --worker
```

The invitation is valid for one hour unless you pass `--ttl` (at most 168 hours). On the machine that will be the Worker:

```sh
cf worker join <invitation>
```

In a terminal this is a wizard: it asks what the flags did not give, offers to install missing harnesses and runs the Worker doctor. Until the doctor is green the Worker takes no Runs. Pass `-` instead of the invitation to read it from stdin and keep it out of your shell history. Then keep the daemon running:

```sh
cf worker run
```

or install it as a service with `cf worker install-service` (systemd user service or launchd).

## Connect another machine as a client

To use CodeFabric from a machine that is not the Controller:

```sh
cf controller token create --client      # on the Controller
cf connect <invitation>                  # on the new machine
```

`cf setup` asks what a machine is for (client, Worker or both) and chains these commands for you. It does not set up the Controller; that is always `cf controller init`.

## Check everything

```sh
cf doctor
```

It checks that the Controller answers, lists every Worker with the verdict of its own doctor, and tries a non-interactive ssh to each Worker, which `cf attach` needs. An unreachable Controller fails the command; a failing component or an ssh that does not work is shown as a warning. On a Worker, `cf worker doctor` gives the detail of its local checks (tmux, git, disk, harnesses and their logins).

## Your first Task

From inside a clone of a repository:

```sh
cf project create          # once per repository; defaults to this directory's origin
cf do "Fix the flaky test in internal/queue" --harness claude-code
```

`cf do` creates a Task from this directory's repository and starts its first Run on a suitable Worker. The harness is one of `claude-code`, `codex` or `command`; the Worker must have it installed and logged in. See [Concepts](../concepts/) for what each one is.

Follow what happens:

```sh
cf ps                      # Runs that are live, and whether they can be attached
cf logs --run <run> -f     # what the harness prints, following
cf attach <task>           # the live session; closing it does not end the Run
cf task show <task>        # the Task and its Runs
```

`cf attach` goes over ssh to the Worker and opens the Run's session. Detach with the keys of the session itself (in tmux, Ctrl-b d); the Run keeps going.

## When a Task needs you

A Run can stop to ask for a permission or an answer. That Task is waiting for input.

```sh
cf inbox
cf task respond <task> --allow
cf task respond <task> --text "use the eu-west region"
```

`cf inbox` lists what needs you: Tasks waiting for input and failures to decide on. `cf task respond` answers: `--allow` or `--deny` for a permission, `--text` for a question. If more than one prompt or Run is waiting, `--prompt` and `--run` pick one.

By default the harness skips its own permission prompts. Create the Task with `--permissions ask` if you want it to ask, so the questions arrive in the inbox.

## The terminal UI

Run `cf` with no arguments in a terminal:

```sh
cf
```

It opens the TUI: home with live counts and activity, then views for Tasks, Workers, Runs, logs and the inbox. See [The TUI](../tui/) for the keys.

## Hand a session to a Worker and bring it back

This is the whole journey of a Claude Code session: from your Mac to a Worker, between Workers, and back to your Mac. Each step names the command and what it prints.

**1. Once per Project: keep the session in checkpoints, and let its Runs use the token.**

```sh
cf project set <project> --native-state on
cf project secrets allow <project> claude/oauth-token
```

The first prints the Project; `Native state` reads `on`. Without it the Project does not accept sessions as seeds. The second prints `Project <id> allows claude/oauth-token.`; without it the handoff is refused with `CF_POLICY_DENIED: harness claude-code needs the secrets claude/oauth-token`.

A working copy belongs to the Project only if its `origin` is exactly one of the Project's URLs. If you clone with SSH and the Project has the HTTPS URL (or the other way round), add the other spelling:

```sh
cf project set <project> --add-repo git@github.com:<owner>/<repo>.git
```

Otherwise `/handto` says `no Project has the origin of this working directory as a repo URL`.

A Run asks for 2 CPUs and 4 GB by default. A Worker that offers less never takes it, and the Run waits in `queued` (`cf run explain` says `memory_mb 4096 > ...`). Lower the default of the Project; the two flags go together:

```sh
cf project set <project> --cpu 2 --memory-mb 2048
```

**2. Once per Worker that may run Claude: give it a token.** Run `claude setup-token` on any machine you are logged in on, then on each such Worker:

```sh
cf worker secrets set claude/oauth-token
```

It reads the token from stdin, or asks for it twice without echo on a terminal, and prints `Stored secret claude/oauth-token (...) ...`. A Worker without the secret is not offered the Run (`CF_SECRET_UNAVAILABLE` in `cf run explain`).

The Worker service finds `claude` and `codex` in its own `PATH`, not in that of your shell: if `cf worker show <worker>` lacks `harness:claude-code` and the log says `harness not offered harness=claude-code status=not-installed`, link the binary into a directory of the service's `PATH`, such as `~/.local/bin` (a tool manager like mise installs it elsewhere), and restart the Worker.

The agent commits as the git user of the Worker. Give each Worker one, or its commits fail with `Author identity unknown`:

```sh
git config --global user.name "<your name>"
git config --global user.email "<your email>"
```

**3. Once on your Mac: install the skill.**

```sh
cf claude install
```

It prints `handto skill: created  ~/.claude/skills/handto/SKILL.md` (`unchanged` if it was already there, `updated` if it was of another version of `cf`). For Codex, `cf codex install` does the same and you ask for it by name (`$handto <worker>`).

**4. Hand the session over.** In Claude Code, in a clone of the Project:

```text
/handto <worker>
```

Claude runs `cf handoff` and the answer is three lines: `Task: <task> (started)`, `Worker: <worker>` and `Continue with: cf attach <task>`. Uncommitted and untracked files travel with it, and your working tree is not touched. The session on your Mac is now marked as handed off: do not keep writing in it. Then close it with `/exit` and follow the work:

```sh
cf attach <task>
```

**5. Move it to another Worker.** From inside the Run, in its session:

```text
/handto <other-worker>
```

It prints `Task <task> is moving to <other-worker> (...)` and the Run of this session ends when the checkpoint is taken; reconnect with `cf attach <task>`. From your Mac, without being in the session, `cf task move <task> <other-worker>` does the same and prints `<task>: move <id> done: the Run on <other-worker> started.` (or the stage it is at).

**6. Bring it back to your Mac.** Suspend the Run and wait for its checkpoint, then pull it, in a clone of the Project:

```sh
cf task defer <task>
cf task pull <task> --open
```

`cf task defer` prints `<task>: capturing a checkpoint; it waits (deferred) once the Worker has published it.` `cf task pull` prints `<task>: branch <branch> (...) is checked out in <dir>.` and `Session <id> installed.` (or `updated`), followed by `Resume it: claude --resume=<id>`; `--open` runs that command for you. `cf task pull <task> --defer` does the defer and the pull in one go.

The branch is the one the Task started from if the checkpoint contains it, and `cf/<task>` otherwise. If your tree has uncommitted changes nothing is touched: the branch is made but not checked out, and the output says `NOT checked out` and offers `cf task pull <task> --worktree <path>`, which makes a worktree for it and installs the session there. The session returns under its own id and the handed-off mark is cleared, so you can hand it over again.

If the Worker of the Run is down, `cf task pull --defer` cannot get a checkpoint: it says so, and that the Run becomes lost when its lease expires, after which `cf task pull <task>` brings the last checkpoint it published.

[The CLI reference](../cli/) has every flag of these commands.

## What is tested, and what is not

The core flow above is covered by automated tests. The default tests use the `command` harness, a scripted fake harness and recorded Claude Code and Codex output. No automated test runs a session with the real Claude Code or Codex CLI; an opt-in check (`CF_REQUIRE_CLAUDE=1`) only confirms the flags the Claude Code adapter relies on. Expect rough edges there, and run `cf worker doctor` first when something does not start.

Of the commands that are present but say so in their help, only `cf run fork` and `promote` are not implemented yet. `cf task defer`, `move` and `resume` work; see below.
