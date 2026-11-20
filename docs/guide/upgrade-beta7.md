---
title: Upgrade to 0.4.0-beta.7
description: The rename to cflk and Code Flock — what changes, the order to upgrade a pool in, and how to go back.
---

0.4.0-beta.7 renames the command to `cflk`, the product to Code Flock, and the files and services that carried the old name. Nothing is lost: the enrolment, the history of Runs, the journal and the secrets move with the directories. What does not move by itself is a running service, so a pool is upgraded in an order.

## What changes

| Before | Now |
| --- | --- |
| `cf` | `cflk`. Where a `cf` of an earlier Code Flock beta was installed, a `cf` that runs `cflk` stays until 0.5. |
| `CF_VERSION`, `CF_INSTALL_DIR`, `CF_CONFIG_DIR`, `CF_DATA_DIR` | `CFLK_VERSION`, `CFLK_INSTALL_DIR`, `CFLK_CONFIG_DIR`, `CFLK_DATA_DIR`. The old ones are read, with a warning, until 0.5; `CFLK_*` wins. |
| `~/.config/codefabric`, `~/.local/share/codefabric` | `~/.config/codeflock`, `~/.local/share/codeflock` |
| `~/Library/Logs/codefabric` (macOS) | `~/Library/Logs/codeflock` |
| systemd `cf-worker`, `cf-controller` | `codeflock-worker`, `codeflock-controller` |
| launchd `com.codefabric.worker`, `com.codefabric.controller` | `com.codeflock.worker`, `com.codeflock.controller` |
| MCP server `cf`, skill `codefabric` | MCP server `cflk`, skill `codeflock` |

These stay as they are: the `cf://` scheme of invitations, `.codefabric/project.yaml` in your repositories, the secrets in the keyring, the `CF_*` error codes and `CF_RUN_SOCKET`.

## Install it

The installer of the beta.6 looks for `cf_…` archives and cannot install the beta.7. Use the installer of the release itself:

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.7/install.sh | CFLK_VERSION=v0.4.0-beta.7 sh
```

`CFLK_INSTALL_DIR` changes where it goes, as in [Getting started](getting-started.md).

The installer puts `cflk` first and checks it runs. Only then does it replace a `cf` of Code Flock 0.4.0-beta or earlier with the one that runs `cflk`. It does not touch a `cf` it cannot tell is ours, such as the Cloud Foundry CLI, and it says so. A `cf` in another directory of your `PATH` is not touched either: if you installed Code Flock in two places, run the installer for each, or remove the one you do not use.

## Before you start

Going back needs what the migration removes, so keep it now, on every machine, clients included. The migration removes the old unit and the beta.7 installer replaces `cf`; the file with its place (`cf-path`) is needed on a client that has no service too.

```sh
mkdir -p ~/cflk-beta6-backup
command -v cf > ~/cflk-beta6-backup/cf-path                                   # where the beta.6 was installed
cp ~/.config/systemd/user/cf-worker.service ~/.config/systemd/user/cf-controller.service ~/cflk-beta6-backup/ 2>/dev/null   # Linux
cp ~/Library/LaunchAgents/com.codefabric.*.plist ~/cflk-beta6-backup/ 2>/dev/null                                           # macOS agent
sudo cp /Library/LaunchDaemons/com.codefabric.*.plist ~/cflk-beta6-backup/ 2>/dev/null                                      # macOS LaunchDaemon
# macOS agent: the domain it is loaded in (gui, or user if installed over ssh); the rollback reads it from here
for d in gui user; do launchctl print $d/$(id -u)/com.codefabric.worker >/dev/null 2>&1 && echo $d/$(id -u) > ~/cflk-beta6-backup/launchd-domain; done
```

If `cf-path` is missing or empty at rollback, find where the beta.6 was (`ls -l` the `cf` your units and `PATH` name; the transition shim is the one to replace) and pass that directory as `CF_INSTALL_DIR` by hand. The copies carry the flags and the environment (`PATH`, `TMUX_TMPDIR`, directories) that `install-service` of the beta.6 would otherwise write again with its defaults.

## The order

A Controller and a Worker that share a machine and a root move together, so the Worker comes first: drain it from an enrolled client still on the beta.6, and wait until no Run is listed on it. The migration refuses, changing nothing, while a Run is live on any Worker that shares the directories.

```sh
cf worker drain <worker>       # add --move to move its Tasks instead of waiting
cf ps                          # wait until no Run is listed on that Worker
```

1. **The Controller.** Install the beta.7 on its machine, then:

   - On Linux, with systemd:

     ```sh
     cflk controller install-service
     ```

     A Worker that shares its root is migrated by this very command, with its own flags and environment. Do not run `cflk worker install-service` for it afterwards: the old unit that supplied its settings is gone.

   - On macOS, `install-service` does not move the Controller: it is only for systemd. Under launchd it changes nothing and prints the commands, in order, to stop the old one, move the directories, write the new property list and load it. The printed block reads the property list of the other job of the old name (`com.codefabric.worker`): if that job uses the same directories and is loaded, the block refuses; if it cannot tell, it warns. A Worker that shares the root has to be drained and stopped first, in the domain it is loaded in, or it would keep the old directories open while the Controller starts on the new ones:

     ```sh
     D=gui/$(id -u); launchctl print $D/com.codefabric.worker >/dev/null 2>&1 || D=user/$(id -u)   # an agent installed over ssh is in user/
     launchctl bootout $D/com.codefabric.worker       # for a LaunchDaemon: sudo launchctl bootout system/com.codefabric.worker
     ```

     Run the block, then migrate that Worker once. A LaunchDaemon: `cflk worker install-service --binary /opt/codeflock/bin/cflk`; its block skips what is already stopped and already moved, writes the new property list and loads it. A LaunchAgent: `cflk worker install-service` writes the new property list, with the environment of the old one, and leaves it stopped: load it with `launchctl bootstrap $D ~/Library/LaunchAgents/com.codeflock.worker.plist`. Do not run `install-service` a second time: it would write a unit from its flags alone and lose that environment. The block removes the old property list: you kept a copy above.
   - Under a supervisor of your own (a system unit, nohup, tmux), the installer and any other `cflk` command leave its directories alone while it runs. Stop the Controller, change its command to `cflk controller run` and start it again: that first run moves the directories. A Worker under a supervisor of your own goes the same way, with `cflk worker run`, once drained.

   Clients and Workers keep connecting without enrolling again: the database, the certificates and the relay move with the directories.

2. **The other Workers, one at a time, drained.** Nothing is upgraded on the clients yet, so drain with the `cf` of the beta.6 as above. Then, on the Worker, after installing the beta.7:

   ```sh
   cflk worker install-service    # it refuses, changing nothing, if a Run is still live
   ```

   Back on the client that drained it (a machine that is only a Worker is not enrolled as a client, and cannot do this):

   ```sh
   cflk worker undrain <worker>   # once that client is on the beta.7; before, cf worker undrain
   cflk worker list               # it is online, with the new version
   ```

   The Worker that moved with the Controller is undrained the same way.

3. **The clients.** Install the beta.7 and run any `cflk` command. With no service of the old name on the machine, the directories move on that first run and it says so once. Then run `cflk claude install`, `cflk codex install` (if you use Codex) and `cflk mcp install --claude` (and `--codex`) again, so the skills and the MCP server are registered as `cflk`.

4. **The completions.** Completions generated for `cf` do not complete `cflk`. Generate them again with `cflk completion <shell>`.

## What `install-service` does with an old unit

When it finds `cf-worker` (or `cf-controller`, or the launchd label of the old name) it:

1. refuses, changing nothing, if a Run is live on the Worker. It says how many and asks you to drain the Worker and wait. The Runs do not follow the move;
2. stops the old unit;
3. moves the directories it used, and on macOS the launchd logs;
4. writes the new unit with what the old one had: its arguments, its `PATH`, its `TMUX_TMPDIR` and the rest of its environment;
5. starts it, and removes the old unit once the new one answers.

If a step fails, it puts the old unit back. A directory you chose by hand with `CF_CONFIG_DIR` or `CF_DATA_DIR` is not moved.

The commands of a launchd plan stop at the first one that fails. Its way back, printed commented out, takes the new service down only if it is loaded and stops if that fails, so it never moves directories under a service that is still running.

A **LaunchDaemon** of the system, in `/Library/LaunchDaemons`, is not managed by `install-service`: it changes nothing and prints the commands, with `sudo`, to stop the old one, move the directories, write the new property list and load it, and the ones that put things back. The daemon runs as root, so its `cflk` must belong to root, in a directory that only root can write to, all the way up. The installer puts `cflk` in your home, which does not qualify: put a copy where it does, and pass it with `--binary`:

```sh
sudo install -d -o root -g wheel -m 755 /opt/codeflock/bin
sudo install -o root -g wheel -m 755 "$(command -v cflk)" /opt/codeflock/bin/cflk
cflk worker install-service --binary /opt/codeflock/bin/cflk
```

(`/usr/local/bin` also works where it belongs to root; with Homebrew on an Intel Mac it usually does not.) Read the printed commands, keep a copy of the old property list if you did not (the commands remove it, and going back needs it), then run them.

If a LaunchDaemon of the beta.6 restarts after the installer has replaced `cf` and before you run those commands, nothing breaks: its `cf` is now the one that runs `cflk`, and `cflk` keeps the old `codefabric` directories while the old property list exists, saying so on stderr. The Worker runs the beta.7 on its old directories until you run the commands.

Until a service is migrated, any other `cflk` command leaves its directories alone and warns, naming the unit and the `install-service` that migrates it.

## Going back

You cannot go back to the beta.6 without undoing the renames: it looks for `codefabric`, finds nothing, and asks you to enrol again. And while `cflk` is on the machine, anything that runs it (a `cflk` command, the MCP server that Claude Code or Codex starts) moves `codefabric` to `codeflock` again whenever no service of the old name is installed. So the order matters: take `cflk` away first, move the directories after.

Do it in this order across a pool, the reverse of the upgrade: the clients, then the Workers, then the Controller. On each machine:

1. **Before anything else, while still on the beta.7.** A client has no unit and nothing to drain: it only removes what the beta.7 registered.
   - On a Worker: drain it from an enrolled client with `cf worker drain <worker>` (the clients are already back on the beta.6), and wait until `cf ps` lists no Run on it. Move the directories back only while no Run is live.
   - Stop **every** new unit that uses the root: the Worker and the Controller, if they share the machine.
   - Remove what the beta.7 registered:

     ```sh
     cflk mcp uninstall --claude    # the MCP server cflk and the skill codeflock
     cflk mcp uninstall --codex
     cflk claude uninstall          # the skills handto and workers
     cflk codex uninstall
     ```

     `--codex` and `cflk codex uninstall` are only needed if you used Codex.

   To stop a unit, `cflk worker uninstall-service` (or `cflk controller uninstall-service`) knows the domain it lives in. By hand, on Linux:

   ```sh
   systemctl --user disable --now codeflock-worker     # and codeflock-controller
   rm ~/.config/systemd/user/codeflock-worker.service
   systemctl --user daemon-reload
   ```

   On macOS, with a launchd agent, the domain is `gui/<uid>`, or `user/<uid>` if the agent was installed over ssh. It was saved in `~/cflk-beta6-backup/launchd-domain` before the migration (a file that is missing means the agent was in neither: check by hand). Read it into `D`, in every shell of the rollback, and stop only when `bootout` succeeds: if it fails, the job is still running, and nothing below is safe. Do not probe for the domain again: once this step has removed the beta.7 job there is nothing left to find it by.

   ```sh
   D=$(cat ~/cflk-beta6-backup/launchd-domain)
   launchctl print $D/com.codeflock.worker >/dev/null 2>&1 || echo "not loaded in gui or user: check by hand before going on"
   launchctl bootout $D/com.codeflock.worker                # if this fails, stop here
   launchctl print $D/com.codeflock.worker                  # must now fail: the job is gone
   rm ~/Library/LaunchAgents/com.codeflock.worker.plist     # only once the line above failed
   ```

   For a LaunchDaemon, do the same with `sudo launchctl bootout system/com.codeflock.worker`, remove `/Library/LaunchDaemons/com.codeflock.worker.plist`, and put back the copy of your old property list you kept. Do not load it yet: it loads in step 4, once the directories are back. A launchd Controller that you moved by hand goes back with the same commands, reversed.

2. **Remove `cflk`, before moving anything:**

   ```sh
   hash -r
   while p=$(command -v cflk); do rm "$p" || break; hash -r; done
   command -v cflk || echo "no cflk on this PATH"
   ```

   This removes every `cflk` on the `PATH` of this shell, which is all it knows about: the transition `cf` runs the first one it finds. If it stops on a copy it cannot remove, remove that one with `sudo rm` (or `brew uninstall codeflock`) and run it again. It does not see a `cflk` that something calls by absolute path. Look at those too: the `ProgramArguments` of your property lists (`/opt/codeflock/bin/cflk` of a LaunchDaemon, for one), the `ExecStart` of your units, and `claude mcp list` and `codex mcp list`, and remove them or point them at the `cf` of the beta.6.

   On a client that never had a service, go on to step 3: with `cflk` gone nothing moves the directories again.

3. **Move the directories back**, never onto one that exists: a plain `mv` onto an existing `codefabric` puts the directory inside it, and the beta.6 then reads the outer one. Use the same roots the beta.7 used (`XDG_CONFIG_HOME` and `XDG_DATA_HOME` if you set them), and stop if the destination is there:

   ```sh
   back() { [ ! -e "$2" ] && [ ! -L "$2" ] || { echo "$2 exists: look at it first" >&2; return 1; }; mv -n "$1" "$2" && [ ! -e "$1" ]; }
   back "${XDG_CONFIG_HOME:-$HOME/.config}/codeflock" "${XDG_CONFIG_HOME:-$HOME/.config}/codefabric"
   back "${XDG_DATA_HOME:-$HOME/.local/share}/codeflock" "${XDG_DATA_HOME:-$HOME/.local/share}/codefabric"
   ```

   On macOS, also `back ~/Library/Logs/codeflock ~/Library/Logs/codefabric`. A directory you chose with `CF_CONFIG_DIR` or `CF_DATA_DIR` never moved: leave it. For a LaunchDaemon, or any service whose directories are not in your home, move back only the ones its new property list names and that sit under a `codeflock` name (its `CFLK_CONFIG_DIR` and `CFLK_DATA_DIR`, or the `codeflock` directories under its `HOME`), with `sudo`, as the account it runs as, with the same `back`.

4. **Install the beta.6 where it was, and put its pieces back.** Its place is in `~/cflk-beta6-backup/cf-path` (a service that ran `/usr/local/bin/cf` needs it there, not in `~/.local/bin`):

   ```sh
   curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.6/install.sh | CF_VERSION=v0.4.0-beta.6 CF_INSTALL_DIR="$(dirname "$(cat ~/cflk-beta6-backup/cf-path)")" sh
   ```

   Then the service, from the copy you kept and not from `install-service`, which would write the defaults again. On Linux:

   ```sh
   cp ~/cflk-beta6-backup/cf-worker.service ~/.config/systemd/user/       # and cf-controller.service
   systemctl --user daemon-reload && systemctl --user enable --now cf-worker
   ```

   On macOS, with a launchd agent: `cp ~/cflk-beta6-backup/com.codefabric.worker.plist ~/Library/LaunchAgents/ && launchctl bootstrap $D ~/Library/LaunchAgents/com.codefabric.worker.plist` (`D` is read from `~/cflk-beta6-backup/launchd-domain`, as in step 1: `D=$(cat ~/cflk-beta6-backup/launchd-domain)` in a new shell). For a LaunchDaemon, put the copy back in `/Library/LaunchDaemons` and `sudo launchctl bootstrap system /Library/LaunchDaemons/com.codefabric.worker.plist`.

   Without a copy, `cf worker install-service` (or `cf controller install-service` on Linux) writes a unit with the defaults: add back the flags and the environment you had.

   ```sh
   cf claude install --force
   cf mcp install --claude        # and --codex
   ```

   On a Worker, from an enrolled client, let it take Runs again with `cf worker undrain <worker>`.

   `--force` is there because the beta.6 treats a skill without its own marker as foreign. The `cf` the beta.7 installer left, if any, is replaced by the beta.6 binary.
