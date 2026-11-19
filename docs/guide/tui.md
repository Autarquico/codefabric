---
title: The TUI
description: The terminal interface of cflk, its views and its keys.
---

Run `cflk` with no arguments in a terminal and it opens the TUI. It needs a connected client (`cflk connect`, or the machine you ran `cflk controller init` on). If either end is not a terminal, or you pass `--json`, `cflk` prints help instead.

The TUI uses the same operations as the CLI. Whatever it does, a `cflk` command does too, and the palette shows which one.

## Home

Home shows the health of the Controller, counts of Tasks and Workers, the state of model usage (it says "usage not reported" when no provider reports any) and the latest activity. The footer lists the keys that open each view:

```text
 [t] Tasks [w] Workers [m] Models [r] Runs [f] Workflows
 [p] Projects [a] Activity [l] Logs [i] Inbox [/] Search
 [ctrl+p] Actions [:] Command [q] Quit
```

## Views

| Key | View | What it is |
| --- | --- | --- |
| `t` | Tasks | Tasks with state, what they wait for and a detail pane with the timeline. |
| `w` | Workers | Workers and their resources; drain, undrain and remove. |
| `m` | Models | Model usage, where providers report it. |
| `r` | Runs | Runs with their state, runtime and Worker. |
| `f` | Workflows | Workflows and their versions; apply, sync and roll back. |
| `p` | Projects | Projects and their defaults. |
| `a` | Activity | The event stream. |
| `l` | Logs | Pick a Run, then follow its output. |
| `i` | Inbox | What needs you: answer without attaching. |
| `/` | Search | On home, listed in the help but not available yet; use `cflk search`. Inside a list, `/` filters. |

## Keys that work everywhere

| Key | Action |
| --- | --- |
| `esc` | Go back. |
| `q`, `ctrl+c` | Quit. |
| `ctrl+p` | Open the palette of actions. |
| `:` | Open the command line. |

## Inside a list

| Key | Action |
| --- | --- |
| `↑` `↓` (or `k` `j`) | Move. |
| `/` | Filter the list; `enter` keeps the filter, `esc` clears it. |
| `enter` | Open the actions for the selected item (not in the Inbox, where it opens the answer form for a question). |

Each view adds its own keys, and shows them in its footer:

- **Tasks:** `a` attach, `s` shell, `c` cancel, `d` complete, `x` archive, `v` live view, `V` grid.
- **Runs:** `a` attach, `s` shell, `P` publish, `v` live view, `V` grid.
- **Workers:** `d` drain, `u` undrain, `x` remove.
- **Workflows:** `A` apply, `s` sync, `R` roll back.
- **Inbox:** `y` allow, `n` deny. Questions open a form for the answer.
- **Logs:** `↑` `↓` scroll, `/` filter, `W` cut or wrap long lines, `G` follow again. Following a Run shows its output as it arrives, and the title says whether you are following, paused, or the Run has ended.

## The palette

`ctrl+p` opens a fuzzy-searchable list of the views and actions you can take. Type to narrow it: typing `drain` finds "Drain worker" and shows the command it stands for, `cflk worker drain`. An action works on the item selected in the view you came from; when it needs one and there is none, it asks. Destructive actions ask for confirmation first.

## The command line

`:` opens a command line that takes the same syntax as the CLI, so what you learn in one carries to the other. `enter` runs it, `↑` `↓` walk the history of what you typed, and `esc` closes it.

## The live view

In Tasks and Runs, `v` turns on a panel that shows the current screen of the selected Run, read-only. It replaces the detail pane while it is on, and its title says which Run it shows, on which Worker, and who is inside (`inside: mbp, omarch01`; a client that is in twice shows once, as `mbp ×2`, and that can be your own machine in another terminal). The panel is off when the TUI starts and stays as you leave it for the rest of the session, in every view that has it.

- It applies to a Run that is interactive and running on a Worker you can reach. In Tasks it follows the Task's live Run. For any other item it shows one line saying why there is no screen.
- It refreshes twice a second, through the Controller. Moving the selection, hiding the panel or leaving the view closes its stream.
- What the Worker captured is cleaned before it is drawn: colours and text stay, and every other terminal sequence (title, clipboard, cursor moves, queries) is dropped. Lines are cut to the panel, not wrapped.
- When the Controller or the Worker refuses, the panel shows the reason instead of the screen: the Run is over (`CF_RUN_NOT_ATTACHABLE`), the Worker is too old for the live view (`CF_NOT_IMPLEMENTED`), or the Worker is offline (`CF_WORKER_OFFLINE`). It does not try again by itself: move the selection, or press `v` twice; the line says so when a retry can help (the Worker offline), and not when it cannot (the Run is over, the Worker is too old).

### The grid

`V`, from Tasks or Runs, opens a grid of the live interactive Runs: one cell each, refreshed once a second, with a title line of Task, Run (named as in the panel), Worker and who is inside. At most six cells are drawn, and the rest are counted (`+1 more, in the Runs view: r-0007 (try 1)`, naming them; the six shown are the first in the order of the Runs view): every cell is a capture on a Worker, and the screen streams of a Worker share a limit of 16. The cells re-flow when the terminal is resized.

| Key | Action |
| --- | --- |
| `←` `→` `↑` `↓` | Move the focus between cells. |
| `enter` | Enter the focused Run, full screen. |
| `esc` | Back to the view you opened the grid from, as you left it. |

## Attaching

`enter` on the panel, or `a` on a Task or Run, enters its live session full screen and interactive, as `cflk attach` does, through the Controller. Everyone attached can type. Leave with the detach key of `cflk`, **Ctrl-]** by default, which the footer of the panel and of the grid shows next to `enter` (`Enter run · Ctrl-] leaves`, with your key if you changed it): you return to the same place in the TUI (the same view, selection, scroll, panel or grid) and the Run goes on. `s` opens a shell in the worktree of the Run instead, as [`cflk shell`](../cli/#cflk-shell) does; it is off unless the Project and the Worker both allow it, and when they do not, the failure line shows the `CF_POLICY_DENIED` message with the policy it names and a hint to lift it: `cflk project set <project> --shell on` (an owner), or `cflk worker config set allow_shell true` on that Worker.

The detach key is the key of `cflk`, not of tmux, so it works when the TUI itself runs inside tmux, provided the tmux around it lets the key through (Ctrl-] is not bound by default; change `detach_key` if yours is). While you are attached, `cflk` reads the key itself and never sends anything to the tmux of your terminal. On the Worker, the Run's tmux still binds `C-]` to detach as a fallback, whatever you configure; keys you type as the session opens wait until its tmux is ready. `cflk` also reads your key when the terminal sends it in its `CSI u` form. The key is set in [`client.json`](../cli/#the-detach-key).

## Terminal size

The layouts are tested at 80x24 (and home at 48x12); the live panel at 120x32 and the grid at 160x40. On smaller terminals, behaviour is not verified.
