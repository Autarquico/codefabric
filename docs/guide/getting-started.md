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

## What is tested, and what is not

The core flow above is covered by automated tests. The default tests use the `command` harness, a scripted fake harness and recorded Claude Code and Codex output. No automated test runs a session with the real Claude Code or Codex CLI; an opt-in check (`CF_REQUIRE_CLAUDE=1`) only confirms the flags the Claude Code adapter relies on. Expect rough edges there, and run `cf worker doctor` first when something does not start.

Several commands are present but say so in their help: `cf task defer`, `move` and `resume`, and `cf run fork` and `promote` are not implemented yet.
