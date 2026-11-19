---
name: handto
description: Hand this Claude Code session over to a CodeFabric Worker, so the work goes on there with the same context and the same repository state. Use only when the user types /handto <worker>.
disable-model-invocation: true
argument-hint: <worker>
arguments: [worker]
allowed-tools: Bash(cflk handoff *) Bash(cflk worker list*)
---
<!-- cflk-version: {{cflk-version}} -->

# /handto

Hand this session over to the CodeFabric Worker `$worker`.

Workers that can take the session right now (online or degraded):

!`cflk worker list --state online,degraded`

## Steps

1. If `$worker` is empty or is not one of the Workers above, say so, show the list and stop. Do not run anything else.
2. Run exactly this command, once:

   `cflk handoff --session ${CLAUDE_SESSION_ID} --to $worker`

3. If it succeeds, show the user the Task, the Worker and the `cflk attach <task>` line that it prints. Then tell them to close this session with `/exit` and to continue with `cflk attach <task>`. Do not keep working in this session: the Worker now has the work.
4. If it fails, show the `CF_*` code and the message exactly as printed, and stop. Do not retry, do not try another command and do not change the arguments. Never add flags to `cflk handoff`. If the error asks for an override, for example because the session holds something that looks like a secret or because it was already handed off, show the user the command with the flag that the message names so that they run it themselves in their own terminal, and do not run it yourself.
