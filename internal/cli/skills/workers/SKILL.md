---
name: workers
description: List the CodeFabric Workers that can take a session right now. Use only when the user types /workers.
disable-model-invocation: true
allowed-tools: Bash(cf worker list*)
---
<!-- cf-version: {{cf-version}} -->

# /workers

Workers that are online or degraded right now, the ones a session can be handed to with `/handto <worker>`:

!`cf worker list --state online,degraded`

## Steps

1. Show the user the list above as it is. Do not run anything else.
2. If it is empty, or the command failed, say so and show the `CF_*` code and the message exactly as printed.
