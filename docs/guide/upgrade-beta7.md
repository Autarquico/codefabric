---
title: Upgrade to 0.4.0-beta.7
description: The rename to cflk and Code Flock — what changes, the order to upgrade a pool in, and how to go back.
---

0.4.0-beta.7 renames the command to `cflk`, the product to Code Flock, and the files and services that carried the old name. Nothing is lost: the enrolment, the history of Runs, the journal and the secrets move with the directories. What does not move by itself is a running service, so a pool is upgraded in an order.

## What changes

| Before | Now |
| --- | --- |
| `cf` | `cflk`. A `cf` that runs `cflk` stays until 0.5. |
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

   ```sh
   cflk controller install-service
   ```

   Clients and Workers keep connecting without enrolling again: the database, the certificates and the relay move with the directories.

2. **The Workers, one at a time, drained.** On each Worker:

   ```sh
   cflk worker drain <worker>     # from any client; wait for its Runs to end
   cflk worker install-service    # on the Worker, after installing the beta.7
   cflk worker undrain <worker>
   cflk worker list               # it is online, with the new version
   ```

3. **The clients.** Install the beta.7 and run any `cflk` command. With no service of the old name on the machine, the directories move on that first run and it says so once. Then run `cflk claude install` and `cflk mcp install --claude` (and `--codex`) again, so the skills and the MCP server are registered as `cflk`.

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

A **LaunchDaemon** of the system, in `/Library/LaunchDaemons`, is not managed by `install-service`: it changes nothing and prints the commands, with `sudo`, to stop the old one, move the directories, write the new property list and load it, and the ones that put things back. Read them, then run them.

Until a service is migrated, any other `cflk` command leaves its directories alone and warns, naming the unit and the `install-service` that migrates it.

## Going back

You cannot go back to the beta.6 without undoing the renames: it looks for `codefabric`, finds nothing, and asks you to enrol again. To go back, stop the new unit, move the directories back and install the beta.6 again. On Linux, for a Worker (the same for a Controller with `codeflock-controller`):

```sh
systemctl --user disable --now codeflock-worker
rm ~/.config/systemd/user/codeflock-worker.service
systemctl --user daemon-reload
mv ~/.config/codeflock ~/.config/codefabric
mv ~/.local/share/codeflock ~/.local/share/codefabric
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.6/install.sh | CF_VERSION=v0.4.0-beta.6 sh
cf worker install-service
```

On macOS, with a launchd agent:

```sh
launchctl bootout gui/$(id -u)/com.codeflock.worker
rm ~/Library/LaunchAgents/com.codeflock.worker.plist
mv ~/.config/codeflock ~/.config/codefabric
mv ~/.local/share/codeflock ~/.local/share/codefabric
mv ~/Library/Logs/codeflock ~/Library/Logs/codefabric
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.4.0-beta.6/install.sh | CF_VERSION=v0.4.0-beta.6 sh
cf worker install-service
```

For a LaunchDaemon, run the `launchctl` commands with `sudo` against `system/com.codeflock.worker` and `/Library/LaunchDaemons`, and put your old property list back.

Move the directories back only while no Run is live. The skills and the MCP server registered by the beta.7 stay; run `cf claude install` and `cf mcp install --claude` with the beta.6 to put its own back.
