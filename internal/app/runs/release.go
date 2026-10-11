package runs

import (
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// releaseRunStopped is the reason a reservation is released when the Worker
// confirms that the Run's processes are gone (§10.5).
const releaseRunStopped = "run_stopped"

// releasable is the one answer to whether the reservation of a Run's lease may
// be released after the Worker confirmed RunStopped (§10.5): the Run must be
// terminal and its runtime stopped.
//
// A Run whose harness never started needs nothing more. For one that did,
// §10.5 also wants the local sealing finished (result_commit persisted or
// publication: failed), and sealing does not exist yet. TEMPORARY EXCEPTION of
// worker-executor-tmux: until git-relay-publication (#12) adds it, a stopped
// terminal Run releases its reservation without it. #12 must tighten this
// branch and invert TestReleasable_StartedRunIsReleasedOnStopUntilSealingExists.
func releasable(run store.Run, neverStarted bool) bool {
	if !run.State.IsTerminal() || run.Runtime != domain.RuntimeStopped {
		return false
	}
	if neverStarted {
		return true
	}
	return true
}
