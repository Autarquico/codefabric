package executor

import (
	"context"

	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/cli/skills"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/runsocket"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// runSocketEnv makes sure an interactive Run has its socket (see RunSockets) and
// returns the variable that tells the harness where it is, "" if the Run has none.
// Only an interactive Run gets one: `/handto` is typed by a person in a session,
// and an autonomous Run has no business asking to be moved. A socket that cannot
// be made is not the Run's failure: the Run goes on without `/handto`.
func (e *Executor) runSocketEnv(ctx context.Context, r state.Run, spec specView) string {
	if e.o.RunSockets == nil || spec.Mode != execwrap.ModeInteractive {
		return ""
	}
	path, err := e.o.RunSockets.Serve(r.RunID, r.LeaseEpoch)
	if err != nil {
		e.log.WarnContext(ctx, "the socket for /handto of a Run cannot be made; the Run goes on without it", "run", r.RunID, "err", err)
		return ""
	}
	return runsocket.EnvVar + "=" + path
}

// runSkills are the skills every Run is given in its configuration so that the
// session inside it can use them. Only Claude Code has one, `/handto`: the Codex
// adapter has no skills directory of its own per Run that is known to be read
// (the plan puts Codex skills in ~/.agents/skills of the host), so a Codex Run
// gets none yet.
func runSkills() []host.MaterializerOption {
	skill, err := skills.HandtoInRun(buildinfo.Get().Version)
	if err != nil { // a version that cannot be written into the skill: the Runs go without it
		return nil
	}
	return []host.MaterializerOption{host.WithFiles(claude.ID, map[string][]byte{"skills/handto/SKILL.md": skill})}
}
