# Harness `command`

The generic harness of the first phase. It runs the prompt of the Run as a
POSIX shell script, with no model behind it. A Run gets it only when it asks
for `requirements.harness: command`. This file documents it as §7.3 of
`docs/codefabric-spec.md` asks every adapter to.

1. **Installation.** There is nothing to install. `Detect` says it is
   installed when the host has an `sh` on its PATH, and reports that path.
   Its Runs are started with `/bin/sh` all the same. `Install` is
   unsupported and not declared.

2. **Configuration paths detected.** None. It reads no configuration, so
   `SnapshotConfig`, `ApplyConfig`, `DiffConfig` and `ValidateConfig` are
   unsupported. It declares no `ConfigIsolation`, so the Worker materialises
   nothing for its Runs: their run directory has no `config/` and no
   `config.json`.

3. **Adoptable components.** None. There is nothing for `cf workflow adopt`
   to read.

4. **Auth.** None. Its `Launch` carries no `SecretEnv`. Whatever the script
   needs it takes from the environment the wrapper gives it, which is the
   daemon's without the tmux variables, as in Phase 1.

5. **Start and resume.** `Start` writes the prompt, followed by a newline,
   to `command.sh` in the run directory, with mode 0700. The write is atomic.
   It returns the argv `/bin/sh <run dir>/command.sh` and no environment of
   its own. Starting the same Run again rewrites the same file and gives the
   same `Launch`. An empty prompt is refused, and so is a mode outside its two
   (`headless` and `interactive`). In interactive mode the script has the
   terminal of the pane. `Resume` is unsupported: a script has no session to
   resume.

6. **Event format.** None. The output of the script is plain text, in
   `output.log` in headless mode and in the pane in interactive mode. `Events`
   is unsupported, and so are `SendInput` and the input signal. The end of the
   Run is the exit code of the script.

7. **Model and usage.** It has no model and no usage. `ModelSelection` and
   `UsageEvents` are false, and `CollectUsage` is unsupported: a Run of
   `command` reports no usage rather than a usage of zero.

8. **Checkpoint and handoff.** There is no native state to export, so
   `Checkpoint` is unsupported. A handoff of a Run of `command` carries only
   what the script left in the worktree. `Stop` is unsupported too: the only
   stop is the signal to the process group.
