---
name: handto
description: Hand this Codex session over to a CodeFabric Worker, so the work goes on there with the same context and the same repository state. Use only when the user explicitly asks to hand this session to a named Worker, for example "$handto omarch02".
---
<!-- cf-version: {{cf-version}} -->

# handto

Hand this session over to the CodeFabric Worker that the user names.

## Steps

1. If the user did not name a Worker, run `cf worker list`, show the list and ask which one. Do not hand anything over until they name it.
2. Run exactly this command, once, with the name of the Worker (the session is the one in `CODEX_SESSION_ID`, which `cf` reads itself):

   `cf handoff --to <worker>`

3. If it succeeds, show the user the Task, the Worker and the `cf attach <task>` line that it prints. Then tell them to close this session and to continue with `cf attach <task>`. Do not keep working in this session: the Worker now has the work.
4. If it fails, show the `CF_*` code and the message exactly as printed, and stop. Do not retry, do not try another command and do not change the arguments. Never add flags to `cf handoff`, and never pass `--session`. If the error asks for an override, for example because the session holds something that looks like a secret or because it was already handed off, show the user the command with the flag that the message names so that they run it themselves in their own terminal, and do not run it yourself.
