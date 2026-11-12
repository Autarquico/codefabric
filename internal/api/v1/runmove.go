package v1

import "github.com/Autarquico/codefabric/internal/domain"

// RunMoveRequest is the body of POST /v1/worker/runs/{run}/move: the Worker of a
// Run asks, for the Run, to move its Task to the Worker named Worker (`/handto`
// from inside a Run). It is the only thing a Run can ask for its Task, and the
// Controller grants it only while the Run is live under that lease. The target
// must be named: a Run does not leave the choice to the scheduler. Nothing here
// can force or retry a move.
type RunMoveRequest struct {
	RequestID  string `json:"request_id,omitempty"`
	LeaseEpoch uint64 `json:"lease_epoch"`
	Worker     string `json:"worker"`
}

// RunPeer is a Worker as a Run may see it, to choose where to move: its name and
// whether it is there. Nothing else about the Worker is told.
type RunPeer struct {
	Name  string             `json:"name"`
	State domain.WorkerState `json:"state"`
}

// RunPeersResponse is the answer of GET /v1/worker/peers.
type RunPeersResponse struct {
	Workers []RunPeer `json:"workers"`
}
