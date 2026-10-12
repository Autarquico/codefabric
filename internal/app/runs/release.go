package runs

import (
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// releaseRunStopped is the reason a reservation is released when the Worker
// confirms that the Run's processes are gone and §10.5 is met.
const releaseRunStopped = "run_stopped"

// releasable is the one answer to whether the reservation of a Run's lease may
// be released (§10.5): the Run is terminal, its processes are gone, no
// evaluator still needs the worktree, and its result is not at risk here.
//
//   - failed, published: the Worker has nothing left to do with the result.
//     A pending publication does not hold the reservation either once the result
//     is sealed (§10.5): publishing is not a reason to keep the CPU.
//   - pending without a result: the sealing is not finished, and sealing
//     needs the worktree the reservation stands for.
//   - none: nothing to seal, for a harness that never ran. A Run that failed in
//     preparation after its session started is the same. A Run cancelled after
//     it started has a worktree nobody sealed: it is kept until stop-cancel-lost-reconcile
//     (#13) makes it pending, since it cannot be told here from one that left
//     nothing behind.
func releasable(run store.Run) bool {
	if !run.State.IsTerminal() || run.Runtime != domain.RuntimeStopped || run.Evaluation == domain.EvaluationPending {
		return false
	}
	switch run.Publication {
	case domain.PublicationFailed, domain.PublicationPublished:
		return true
	case domain.PublicationPending:
		return run.ResultCommit != ""
	default:
		return run.StartedAt == nil || run.EndReason == domain.EndEnvironment
	}
}
