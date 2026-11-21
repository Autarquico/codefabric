---
title: Getting started
description: Install cflk, start a Controller, join a Worker and run your first Task.
---

This page takes you from nothing to a Task running on a Worker. It assumes a Git repository you can push to, and one machine to start with: the Controller, a Worker and the client can all be the same machine.

## Install

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.7/install.sh | CFLK_VERSION=v0.4.0-beta.7 sh
```

`releases/latest` skips prereleases, so while 0.4.0-beta.7 is the newest release of `cflk`, `…/releases/latest/download/install.sh` installs an older release that still calls the command `cf`: pin the version, as above, until a stable release has `cflk`.

The installer puts `cflk` in `~/.local/bin`. Two variables change that:

| Variable | Meaning |
| --- | --- |
| `CFLK_VERSION` | The version to install, such as `v0.4.0-beta.7`. Default: the latest stable release, which is not yet one with `cflk`: set it. |
| `CFLK_INSTALL_DIR` | Where to put `cflk`. Default: `~/.local/bin`. |

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.7/install.sh | CFLK_VERSION=v0.4.0-beta.7 CFLK_INSTALL_DIR=/usr/local/bin sh
```

The installer of a release is the only one that knows that release's files, so a pinned version takes `install.sh` from its own tag. Releases before 0.4.0-beta.7 need the installer of their own release.

The old `CF_VERSION`, `CF_INSTALL_DIR` and `CF_RELEASE_BASE_URL` are still read, with a warning, until 0.5; `CFLK_*` wins when both are set. If the install directory already has the `cf` of an earlier Code Flock beta, the installer replaces it with a `cf` that runs `cflk`, so existing scripts keep working until 0.5. Any other `cf`, such as the Cloud Foundry CLI, is left alone, and a fresh install has only `cflk`.

Upgrading from 0.4.0-beta.6 or earlier? Read [Upgrade to 0.4.0-beta.7](../upgrade-beta7/) first: the installer of the beta.6 cannot install it.

Supported platforms: Linux (amd64, arm64) and macOS (amd64, arm64). Anything else is refused before downloading.

Before it installs anything, the script checks an OpenSSH signature on the list of checksums, then the checksum of the archive. It needs OpenSSH 8.2 or later (`ssh-keygen`). If a check fails, nothing is written to the install directory. At the end it tells you whether the directory is on your `PATH` and suggests a next step; it does not run anything else.

:::note[What has not been verified]
The installer and its signing chain are tested in CI against a throwaway key. It has not yet been verified on a clean machine against a published release.
:::

Check it works:

```sh
cflk version
```

## The network between machines

Every Worker and client dials the Controller at its advertised address (port 7443 by default), and a client attaches to a Run through the Controller (`cflk attach`), or over ssh to the Worker with `cflk attach --ssh`. The recommended network is a [Tailscale](https://tailscale.com) tailnet: the machines reach each other by name wherever they are. Decide it before you start the Controller: these flags go on its first `cflk controller init`. Advertise the Controller's tailnet name, since its host goes into the Controller's certificate, and bind it to the tailnet address so the API is not reachable from anywhere else:

```sh
cflk controller init --advertise <controller>.<tailnet>.ts.net:7443 --listen <tailnet-ip>:7443
```

Without `--listen` the Controller listens on every interface, on the advertised port (`0.0.0.0:7443` by default); on a machine with a public address, keep that port closed with a firewall.

A Worker tells clients to reach it over ssh as `<user>@<hostname>`, which is often not its tailnet name (on a Mac, `Name-MacBook-Pro.local`). Give it its tailnet name when you install or run it, with the same flag on `cflk worker run`:

```sh
cflk worker install-service --ssh-target <user>@<worker>.<tailnet>.ts.net
```

Tailscale only carries the traffic. The Controller's API still authenticates every Worker and client with its own certificates (mutual TLS), inside the tailnet too. `cflk attach --ssh` uses the OpenSSH client: plain ssh authenticates with your keys, Tailscale SSH with your tailnet identity and its policy.

If you use Tailscale SSH in `check` mode, it asks for a login in the browser from time to time. Until you do, the ssh check of `cflk doctor` warns that it cannot reach that Worker, and `cflk attach --ssh` waits for that login.

## Start a Controller

The Controller is the one process that holds the state. Create it once, then run it:

```sh
cflk controller init
cflk controller run
```

`init` creates the database, a certificate authority, certificates and the credentials of the machine you are on. By default clients dial `<hostname>:7443`; use `--advertise host:port` if other machines reach this one by another name. `run` serves the API until you stop it. To keep it running, `cflk controller install-service` sets it up as a systemd user service on Linux.

`cflk controller init` also creates the owner credentials and a name for the owner's machine (`--name`). Once the Controller is running, `cflk status` shows its version, uptime and database health.

## Add a Worker

A Worker is a machine that runs Tasks. On the Controller's machine, create a single-use invitation:

```sh
cflk controller token create --worker
```

The invitation is valid for one hour unless you pass `--ttl` (at most 168 hours). On the machine that will be the Worker:

```sh
cflk worker join <invitation>
```

In a terminal this is a wizard: it asks what the flags did not give, offers to install missing harnesses and runs the Worker doctor. Until the doctor is green the Worker takes no Runs. Pass `-` instead of the invitation to read it from stdin and keep it out of your shell history. Then keep the daemon running:

```sh
cflk worker run
```

or install it as a service with `cflk worker install-service` (systemd user service `codeflock-worker`, or launchd `com.codeflock.worker`).

On macOS `install-service` installs a launchd agent of your user. With nobody logged in it goes into the `user/<uid>` domain, with a warning: it runs, but after a restart launchd does not load it again until you log in. Over SSH, while someone is logged in at the screen, launchd refuses `gui/<uid>` with `exit status 125`: `install-service` then falls back to `user/<uid>` with the same warning. For a Worker that starts at boot without a login, run it as a LaunchDaemon in `/Library/LaunchDaemons/com.codeflock.worker.plist` (owned by `root:wheel`, mode 644). Keep `AbandonProcessGroup`: without it, restarting the daemon kills the tmux sessions of its Runs.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.codeflock.worker</string>
  <key>UserName</key><string>YOUR_USER</string>
  <key>ProgramArguments</key>
  <array><string>/Users/YOUR_USER/.local/bin/cflk</string><string>worker</string><string>run</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>/Users/YOUR_USER</string>
    <key>PATH</key><string>/Users/YOUR_USER/.local/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>WorkingDirectory</key><string>/Users/YOUR_USER</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>AbandonProcessGroup</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>/Users/YOUR_USER/Library/Logs/codeflock/com.codeflock.worker.log</string>
  <key>StandardErrorPath</key><string>/Users/YOUR_USER/Library/Logs/codeflock/com.codeflock.worker.log</string>
</dict>
</plist>
```

Load it with `sudo launchctl bootstrap system /Library/LaunchDaemons/com.codeflock.worker.plist` and restart it with `sudo launchctl kickstart -k system/com.codeflock.worker`. `cflk` ends cleanly on a signal, so launchd does not start it again after one: restart it with `kickstart`.

## Connect another machine as a client

To use Code Flock from a machine that is not the Controller:

```sh
cflk controller token create --client      # on the Controller
cflk connect <invitation>                  # on the new machine
```

`cflk setup` asks what a machine is for (client, Worker or both) and chains these commands for you. It does not set up the Controller; that is always `cflk controller init`.

## Check everything

```sh
cflk doctor
```

It checks that the Controller answers, lists every Worker with the verdict of its own doctor, and tries a non-interactive ssh to each Worker, which `cflk attach --ssh` needs. An unreachable Controller fails the command; a failing component or an ssh that does not work is shown as a warning. On a Worker, `cflk worker doctor` gives the detail of its local checks (tmux, git, disk, harnesses and their logins).

## Your first Task

From inside a clone of a repository:

```sh
cflk project create          # once per repository; defaults to this directory's origin
cflk do "Fix the flaky test in internal/queue" --harness claude-code
```

`cflk do` creates a Task from this directory's repository and starts its first Run on a suitable Worker. The harness is one of `claude-code`, `codex` or `command`; the Worker must have it installed and logged in. See [Concepts](../concepts/) for what each one is.

Follow what happens:

```sh
cflk ps                      # Runs that are live, and whether they can be attached
cflk logs --run <run> -f     # what the harness prints, following
cflk attach <task>           # the live session; closing it does not end the Run
cflk task show <task>        # the Task and its Runs
```

`cflk attach` opens the Run's session through the Controller, with no ssh to the Worker. Detach with Ctrl-] (configurable, see [the CLI reference](../cli/#the-detach-key)); the Run keeps going.

## When a Task needs you

A Run can stop to ask for a permission or an answer. That Task is waiting for input.

```sh
cflk inbox
cflk task respond <task> --allow
cflk task respond <task> --text "use the eu-west region"
```

`cflk inbox` lists what needs you: Tasks waiting for input and failures to decide on. `cflk task respond` answers: `--allow` or `--deny` for a permission, `--text` for a question. If more than one prompt or Run is waiting, `--prompt` and `--run` pick one.

By default the harness skips its own permission prompts. Create the Task with `--permissions ask` if you want it to ask, so the questions arrive in the inbox.

## The terminal UI

Run `cflk` with no arguments in a terminal:

```sh
cflk
```

It opens the TUI: home with live counts and activity, then views for Tasks, Workers, Runs, logs and the inbox. See [The TUI](../tui/) for the keys.

## Hand a session to a Worker and bring it back

This is the whole journey of a Claude Code session: from your Mac to a Worker, between Workers, and back to your Mac. Each step names the command and what it prints.

**1. Once per Project: keep the session in checkpoints, and let its Runs use the token.**

```sh
cflk project set <project> --native-state on
cflk project secrets allow <project> claude/oauth-token
```

The first prints the Project; `Native state` reads `on`. Without it the Project does not accept sessions as seeds. The second prints `Project <id> allows claude/oauth-token.`; without it the handoff is refused with `CF_POLICY_DENIED: harness claude-code needs the secrets claude/oauth-token`.

A working copy belongs to the Project only if its `origin` is exactly one of the Project's URLs. If you clone with SSH and the Project has the HTTPS URL (or the other way round), add the other spelling:

```sh
cflk project set <project> --add-repo git@github.com:<owner>/<repo>.git
```

Otherwise `/handto` says `no Project has the origin of this working directory as a repo URL`.

A Run asks for 2 CPUs and 4 GB by default. A Worker that offers less never takes it, and the Run waits in `queued` (`cflk run explain` says `memory_mb 4096 > ...`). Lower the default of the Project; the two flags go together:

```sh
cflk project set <project> --cpu 2 --memory-mb 2048
```

**2. Once per Worker that may run Claude: give it a token.** Run `claude setup-token` on any machine you are logged in on. It prints more than the token, and `setup-token` is interactive, so it cannot be piped. The most reliable way is to save the whole output to a file and feed that: `cflk` keeps the `sk-ant-oat` line, joins it if the terminal wrapped it, and says so. To keep the token out of your shell's history, never put it on the command line: use a file or the clipboard (`pbpaste | cflk worker secrets set claude/oauth-token` on macOS).

At the prompt, paste only the line that starts with `sk-ant-oat`. The prompt does not show what you paste. A paste that goes over several lines is one answer, whether its lines end in LF, CR or CRLF: a line end ends the answer only when nothing follows it within about 50 ms, so a wrapped paste is joined, with or without a final line end. In a terminal with bracketed paste the whole paste is kept whatever its timing and your own Enter ends the answer; in one without it, a paste that includes its final line end already ends the answer, so do not press Enter after it. If the two pastes still differ, `cflk` says so and stores nothing: give the token from a file or the clipboard as above.

```sh
cflk worker secrets set claude/oauth-token
# or, from a file with the output of setup-token:
cflk worker secrets set claude/oauth-token < setup-token.txt
```

It reads the token from stdin, or asks for it twice without echo on a terminal, and prints `Stored secret claude/oauth-token (...) ...`. A Worker without the secret is not offered the Run (`CF_SECRET_UNAVAILABLE` in `cflk run explain`). Only the Claude token is joined from several lines: for any other secret the prompt refuses an answer of several lines and stores nothing, so give a value of several lines, or one the terminal may wrap, on stdin.

If a token was stored badly with an earlier version of `cflk` (for example as two lines, or the code from the browser), `cflk worker doctor` cannot tell, and a Run fails with `401 Invalid bearer token`: set it again with the command above.

The value must look like a token (`sk-ant-oat…`): anything else, such as the code the browser shows, is refused with `CF_INVALID_REQUEST` and nothing is stored. `--force` stores it anyway and warns on stderr that Runs will likely fail with 401; `cflk worker doctor` then warns on `auth:claude-code` until you set it again with the line `claude setup-token` prints.

The Worker service finds `claude` and `codex` in its own `PATH`, not in that of your shell: if `cflk worker show <worker>` lacks `harness:claude-code` and the log says `harness not offered harness=claude-code status=not-installed`, link the binary into a directory of the service's `PATH`, such as `~/.local/bin` (a tool manager like mise installs it elsewhere), and restart the Worker.

The agent commits as the git user of the Worker. Give each Worker one, or its commits fail with `Author identity unknown`:

```sh
git config --global user.name "<your name>"
git config --global user.email "<your email>"
```

**3. Once on your Mac: install the skill.**

```sh
cflk claude install
```

It prints `handto skill: created  ~/.claude/skills/handto/SKILL.md` (`unchanged` if it was already there, `updated` if it was of another version of `cflk`). For Codex, `cflk codex install` does the same and you ask for it by name (`$handto <worker>`).

**4. Hand the session over.** In Claude Code, in a clone of the Project:

```text
/handto <worker>
```

Claude runs `cflk handoff` and the answer is three lines: `Task: <task> (started)`, `Worker: <worker>` and `Continue with: cflk attach <task>`. Uncommitted and untracked files travel with it, and your working tree is not touched. The session on your Mac is now marked as handed off: do not keep writing in it. Then close it with `/exit` and follow the work:

```sh
cflk attach <task>
```

**5. Move it to another Worker.** From inside the Run, in its session:

```text
/handto <other-worker>
```

It prints `Task <task> is moving to <other-worker> (...)` and the Run of this session ends when the checkpoint is taken; reconnect with `cflk attach <task>`. From your Mac, without being in the session, `cflk task move <task> <other-worker>` does the same and prints `<task>: move <id> done: the Run on <other-worker> started.` (or the stage it is at).

**6. Bring it back to your Mac.** Suspend the Run and wait for its checkpoint, then pull it, in a clone of the Project:

```sh
cflk task defer <task>
cflk task pull <task> --open
```

`cflk task defer` prints `<task>: capturing a checkpoint; it waits (deferred) once the Worker has published it.` `cflk task pull` prints `<task>: branch <branch> (...) is checked out in <dir>.` and `Session <id> installed.` (or `updated`), followed by `Resume it: claude --resume=<id>`; `--open` runs that command for you. `cflk task pull <task> --defer` does the defer and the pull in one go.

The branch is the one the Task started from if the checkpoint contains it, and `cf/<task>` otherwise. If your tree has uncommitted changes nothing is touched: the branch is made but not checked out, and the output says `NOT checked out` and offers `cflk task pull <task> --worktree <path>`, which makes a worktree for it and installs the session there. The session returns under its own id and the handed-off mark is cleared, so you can hand it over again.

If the Worker of the Run is down, `cflk task pull --defer` cannot get a checkpoint: it says so, and that the Run becomes lost when its lease expires, after which `cflk task pull <task>` brings the last checkpoint it published.

[The CLI reference](../cli/) has every flag of these commands.

## What is tested, and what is not

The core flow above is covered by automated tests. The default tests use the `command` harness, a scripted fake harness and recorded Claude Code and Codex output. No automated test runs a session with the real Claude Code or Codex CLI; an opt-in check (`CF_REQUIRE_CLAUDE=1`) only confirms the flags the Claude Code adapter relies on. Expect rough edges there, and run `cflk worker doctor` first when something does not start.

Of the commands that are present but say so in their help, only `cflk run fork` and `promote` are not implemented yet. `cflk task defer`, `move` and `resume` work; see below.
