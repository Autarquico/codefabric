---
title: The TUI
description: The terminal interface of cf, its views and its keys.
---

Run `cf` with no arguments in a terminal and it opens the TUI. It needs a connected client (`cf connect`, or the machine you ran `cf controller init` on). If either end is not a terminal, or you pass `--json`, `cf` prints help instead.

The TUI uses the same operations as the CLI. Whatever it does, a `cf` command does too, and the palette shows which one.

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
| `/` | Search | Listed in the help, but not available yet; use `cf search`. |

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
| `enter` | Open the actions for the selected item. |

Each view adds its own keys, and shows them in its footer:

- **Tasks:** `a` attach, `c` cancel, `d` complete, `x` archive.
- **Runs:** `a` attach, `P` publish.
- **Workers:** `d` drain, `u` undrain, `x` remove.
- **Workflows:** `A` apply, `s` sync, `R` roll back.
- **Inbox:** `y` allow, `n` deny. Questions open a form for the answer.
- **Logs:** `↑` `↓` scroll, `/` filter, `W` cut or wrap long lines, `G` follow again. Following a Run shows its output as it arrives, and the title says whether you are following, paused, or the Run has ended.

## The palette

`ctrl+p` opens a fuzzy-searchable list of the views and actions you can take. Type to narrow it: typing `drain` finds "Drain worker" and shows the command it stands for, `cf worker drain`. An action works on the item selected in the view you came from; when it needs one and there is none, it asks. Destructive actions ask for confirmation first.

## The command line

`:` opens a command line that takes the same syntax as the CLI, so what you learn in one carries to the other. `enter` runs it, `↑` `↓` walk the history of what you typed, and `esc` closes it.

## Attaching

`a` on a Task or Run attaches to its live session, as `cf attach` does. Detach with the keys of the session itself (in tmux, Ctrl-b d); the Run goes on, and you return to the TUI.

## Terminal size

The layouts are tested at 80x24 (and home at 48x12). On smaller terminals, behaviour is not verified.
