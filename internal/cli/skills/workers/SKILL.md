---
name: workers
description: List the Code Flock Workers and the interactive Runs that are live on them, and say how to enter one. Use only when the user types /workers.
disable-model-invocation: true
allowed-tools: Bash(cflk worker list --timeout 10s) Bash(cflk ps --timeout 10s)
---
<!-- cflk-version: {{cflk-version}} -->

# /workers

Workers, with their state. Only an `online` or `degraded` Worker can take a session with `/handto <worker>`; one that is `offline` cannot:

!`cflk worker list --timeout 10s`

Runs that are live on those Workers. The ones with MODE `interactive` are the remote sessions, for example a session sent with `/handto`:

!`cflk ps --timeout 10s`

## Steps

1. Show the Workers above as they are, with their state, so an offline one is plain to see.
2. Under them, list the Runs with MODE `interactive` as remote, each with its WORKER and its TASK. If there are none, say so.
3. For each one, tell the user the exact command to enter it, to run in another terminal: `cflk attach <task>`, with the TASK of that Run. Never run `cflk attach` yourself and do not touch this session: it takes over a terminal.
4. If a command failed, say so and show the `CF_*` code and the message exactly as printed. Do not run anything else.
