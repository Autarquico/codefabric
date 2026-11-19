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

`CFLK_INSTALL_DIR` changes where it goes, as in [Getting started](../getting-started/).

The installer puts `cflk` first and checks it runs. Only then does it replace a `cf` of Code Flock 0.4.0-beta or earlier with the one that runs `cflk`. It does not touch a `cf` it cannot tell is ours, such as the Cloud Foundry CLI, and it says so. A `cf` in another directory of your `PATH` is not touched either: if you installed Code Flock in two places, run the installer for each, or remove the one you do not use.

## The order

1. **The Controller.** Install the beta.7 on its machine, then:

   - On Linux, with systemd:

     ```sh
     cflk controller install-service
     ```

   - On macOS, `install-service` does not move the Controller: it is only for systemd. Under launchd it changes nothing and prints the commands, in order, to stop the old one, move the directories, write the new property list and load it. Read them, then run them. Keep a copy of the old property list: the last of those commands removes it, and going back needs it.
   - Under a supervisor of your own, stop the Controller, change its command to `cflk controller run` and start it again. With no service of the old name installed, the first `cflk` that runs moves the directories.

   Clients and Workers keep connecting without enrolling again: the database, the certificates and the relay move with the directories.

2. **The Workers, one at a time, drained.** Nothing is upgraded on the clients yet, so drain with the `cf` of the beta.6:

   ```sh
   cf worker drain <worker>       # from a client still on the beta.6; add --move to move its Tasks instead of waiting
   cf ps                          # wait until no Run is listed on that Worker
   ```

   Then, on the Worker, after installing the beta.7:

   ```sh
   cflk worker install-service    # it refuses, changing nothing, if a Run is still live
   cflk worker undrain <worker>
   cflk worker list               # it is online, with the new version
   ```

3. **The clients.** Install the beta.7 and run any `cflk` command. With no service of the old name on the machine, the directories move on that first run and it says so once. Then run `cflk claude install`, `cflk codex install` (if you use Codex) and `cflk mcp install --claude` (and `--codex`) again, so the skills and the MCP server are registered as `cflk`.

4. **The completions.** Completions generated for `cf` do not complete `cflk`. Generate them again with `cflk completion <shell>`.

If a Controller and a Worker share a machine and a root, they migrate together.

## What `install-service` does with an old unit

When it finds `cf-worker` (or `cf-controller`, or the launchd label of the old name) it:

1. refuses, changing nothing, if a Run is live on the Worker. It says how many and asks you to drain the Worker and wait. The Runs do not follow the move;
2. stops the old unit;
3. moves the directories it used, and on macOS the launchd logs;
4. writes the new unit with what the old one had: its arguments, its `PATH`, its `TMUX_TMPDIR` and the rest of its environment;
5. starts it, and removes the old unit once the new one answers.

If a step fails, it puts the old unit back. A directory you chose by hand with `CF_CONFIG_DIR` or `CF_DATA_DIR` is not moved.

A **LaunchDaemon** of the system, in `/Library/LaunchDaemons`, is not managed by `install-service`: it changes nothing and prints the commands, with `sudo`, to stop the old one, move the directories, write the new property list and load it, and the ones that put things back. Read them, keep a copy of the old property list (the commands remove it, and going back needs it), then run them.

If a LaunchDaemon of the beta.6 restarts after the installer has replaced `cf` and before you run those commands, nothing breaks: its `cf` is now the one that runs `cflk`, and `cflk` keeps the old `codefabric` directories while the old property list exists, saying so on stderr. The Worker runs the beta.7 on its old directories until you run the commands.

Until a service is migrated, any other `cflk` command leaves its directories alone and warns, naming the unit and the `install-service` that migrates it.

## Going back

You cannot go back to the beta.6 without undoing the renames: it looks for `codefabric`, finds nothing, and asks you to enrol again. And while `cflk` is on the machine, anything that runs it (a `cflk` command, the MCP server that Claude Code or Codex starts) moves `codefabric` to `codeflock` again whenever no service of the old name is installed. So the order matters: take `cflk` away first, move the directories after.

Do it in this order across a pool, the reverse of the upgrade: the clients, then the Workers, then the Controller. On each machine:

1. **Before anything else, while still on the beta.7.** A client has no unit and nothing to drain: it only removes what the beta.7 registered.
   - On a Worker: drain it from a client with `cf worker drain <worker>` (the clients are already back on the beta.6), and wait until `cf ps` lists no Run on it. Move the directories back only while no Run is live.
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

   On macOS, with a launchd agent, the domain is `gui/<uid>`, or `user/<uid>` if the agent was installed over ssh. Try both:

   ```sh
   launchctl bootout gui/$(id -u)/com.codeflock.worker    # or user/$(id -u)/com.codeflock.worker
   rm ~/Library/LaunchAgents/com.codeflock.worker.plist
   ```

   For a LaunchDaemon, run the same `launchctl bootout` with `sudo` against `system/com.codeflock.worker`, remove `/Library/LaunchDaemons/com.codeflock.worker.plist`, and put back the copy of your old property list you kept when you upgraded. A launchd Controller that you moved by hand goes back with the same commands, reversed.

2. **Remove `cflk`, before moving anything:**

   ```sh
   while command -v cflk >/dev/null; do rm "$(command -v cflk)"; done
   ```

   This removes every `cflk` on your `PATH`: the transition `cf` runs the first one it finds.

   On a client that never had a service, go on to step 3: with `cflk` gone nothing moves the directories again.

3. **Move the directories back.** On Linux:

   ```sh
   mv ~/.config/codeflock ~/.config/codefabric
   mv ~/.local/share/codeflock ~/.local/share/codefabric
   ```

   On macOS, also `mv ~/Library/Logs/codeflock ~/Library/Logs/codefabric`.

4. **Install the beta.6 and put its pieces back:**

   ```sh
   curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.6/install.sh | CF_VERSION=v0.4.0-beta.6 sh
   cf worker install-service      # on a Worker; on a Controller, cf controller install-service (Linux)
   cf claude install --force
   cf mcp install --claude        # and --codex
   ```

   On a Worker, from a client, let it take Runs again with `cf worker undrain <worker>`.

   `--force` is there because the beta.6 treats a skill without its own marker as foreign. The `cf` the beta.7 installer left, if any, is replaced by the beta.6 binary.
