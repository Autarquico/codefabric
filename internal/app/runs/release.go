package runs

import (
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// releaseRunStopped is the reason a reservation is released when the Worker
// confirms that the Run's processes are gone (§10.5).
const releaseRunStopped = "run_stopped"

// releaseStartedWithoutSealing is the TEMPORARY EXCEPTION of
// worker-executor-tmux (#11): sealing does not exist yet, so a Run whose harness
// started is released at RunStopped without it. git-relay-publication (#12) sets
// it to false and makes releasable ask for the sealing.
const releaseStartedWithoutSealing = true

// releasable is the one answer to whether the reservation of a Run's lease may
// be released after the Worker confirmed RunStopped (§10.5): the Run must be
// terminal and its runtime stopped. A Run whose harness never started needs
// nothing more; for one that did, §10.5 also wants the local sealing finished.
func releasable(run store.Run, neverStarted bool) bool {
	if !run.State.IsTerminal() || run.Runtime != domain.RuntimeStopped {
		return false
	}
	return neverStarted || releaseStartedWithoutSealing
}
