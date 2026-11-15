---
name: workers
description: List the CodeFabric Workers that can take a session right now. Use only when the user explicitly asks which Workers are available, for example "$workers".
---
<!-- cf-version: {{cf-version}} -->

# workers

List the Workers that are online or degraded, the ones a session can be handed to with the handto skill.

## Steps

1. Run exactly this command, once:

   `cf worker list --state online,degraded`

2. Show the user the list as it is. Do not run anything else.
3. If it is empty, or the command failed, say so and show the `CF_*` code and the message exactly as printed.
