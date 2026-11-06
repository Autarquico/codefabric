---
title: Concepts
description: The parts of CodeFabric and how they relate, in the words the CLI and the TUI use.
---

CodeFabric runs coding agents across a pool of machines you own. You say what you want done; it finds a machine, runs an agent there, and keeps the record.

## Machines

**Controller.** The one process that holds the state: Projects, Tasks, Runs, events and the scheduler. It has a database, its own certificate authority and an API. Workers and clients talk to it over mutually authenticated TLS. There is one per installation (`cf controller init`, `cf controller run`).

**Worker.** A machine that runs Runs. It runs a daemon (`cf worker run`), announces which harnesses it has, keeps a clone of each repository it has worked on and does the work in isolated Git worktrees. You choose how much of the machine it contributes (CPUs, memory, disk, concurrent Runs) and when it accepts work (`always`, `idle` or `manual`). A Worker can be drained so that it takes no new Runs and the ones it has go on.

**Client.** A machine from which you use CodeFabric: the `cf` commands and the TUI. Clients enrol with an invitation (`cf connect`). One machine can be a client, a Worker, or both, with separate credentials for each.

Invitations are single-use and short-lived: `cf controller token create --client` or `--worker`.

## Work

**Project.** A repository plus the defaults for work on it: the default branch, completion policy and so on. A Project's configuration can come from several layers, and `cf project explain` shows the effective value of each field and where it came from.

**Task.** What you want achieved, independent of where it runs: a name, a prompt, a Project and a starting ref. A Task has a state (`queued`, `running`, `waiting`, `completed`, `failed`, `cancelled`, `archived`). A waiting Task says why: it has not started, it needs input, it was deferred, it needs a decision, or it needs review. Its completion policy says what makes it done: `first_success`, `promote` or `manual`.

**Run.** One attempt at a Task, on one Worker, in one worktree, with one harness. A Task can have several Runs, for example when a Run fails and is retried. A Run's state includes `queued`, `preparing`, `running`, `waiting_input`, `completed`, `failed`, `cancelled` and `lost`. The result of a Run is published as a branch. If publication fails, `cf run publish` tries again.

`cf run explain` says why a Run is waiting or where it went, Worker by Worker, so scheduling is never a black box.

## Needs input and the inbox

A harness can stop to ask: a permission (may I run this command?) or a question. The Run moves to `waiting_input` and the Task shows as waiting for input. You can answer without attaching:

- `cf inbox` or the inbox view of the TUI list what needs you.
- `cf task respond` answers, or `y` and `n` in the TUI inbox.

The inbox also lists failures that need a decision. By default a harness skips its own permission prompts; create the Task with `--permissions ask` to have them asked.

## Harnesses

A harness is the program that drives the coding agent. CodeFabric does not replace it, and does not translate its configuration into a common format. Each has its own adapter.

| Harness | What it is |
| --- | --- |
| `claude-code` | Claude Code, the `claude` program. |
| `codex` | OpenAI's Codex CLI, the `codex` program. |
| `command` | A generic harness: the prompt is run as a POSIX shell script. No model is involved. Useful to check the plumbing. |

A Worker offers the harnesses it has installed. `cf worker doctor` checks that they are present and logged in, and `cf worker join` can offer to install the missing ones.

The adapters for `claude-code` and `codex` are tested against the real programs only when you opt in; the default test run uses recorded behaviour. The `command` harness is covered in the default run.

## Configuration that travels

**Workflow.** A versioned, portable bundle of the native configuration of your harnesses and tools (for example the settings and instructions of Claude Code), together with the rules for putting it on a machine. It is not a universal format: it holds native snapshots, metadata and who owns each part. `cf workflow adopt` creates one from the configuration already on your machine; `cf workflow diff` and `explain` show what applying it would do; `apply` and `sync` put it on one or every Worker; `status` shows drift; `rollback` goes back one version.

**Environment.** How a worktree is prepared for a Run: the runtimes, services and dependencies it needs. It is versioned the same way (`cf environment apply`, `show`, `list`). It does not impose Docker, Nix or any other tool.

## Observing

Everything that happens is an event. `cf events` shows them and follows them; `cf logs` shows the structured logs of the Controller and the Workers, or the exact output of one Run. `cf search` finds Projects, Tasks, Runs, Workers and Workflows by reference or text. `cf notify` turns the events that need you into desktop notifications and hooks.

References accept a name or a short ID. When a Task name exists in more than one Project, `--project` says which.
