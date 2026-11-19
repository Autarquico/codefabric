---
name: workers
description: List the CodeFabric Workers and the interactive Runs that are live on them, and say how to enter one. Use only when the user explicitly asks which Workers are available or which remote sessions are running, for example "$workers".
---
<!-- cflk-version: {{cflk-version}} -->

# workers

List the Workers with their state, and the interactive Runs that are live on them. Only an `online` or `degraded` Worker can take a session with the handto skill.

## Steps

1. Run exactly these commands, once each:

   `cflk worker list --timeout 10s`

   `cflk ps --timeout 10s`

2. Show the Workers as they are, with their state, so an offline one is plain to see.
3. Under them, list the Runs with MODE `interactive` as remote, each with its WORKER and its TASK. If there are none, say so.
4. For each one, tell the user the exact command to enter it, to run in another terminal: `cflk attach <task>`, with the TASK of that Run. Never run `cflk attach` yourself.
5. If a command failed, say so and show the `CF_*` code and the message exactly as printed. Do not run anything else.
