package domain

// WorkerCommandType is a kind of order in the Controller→Worker queue. Only
// the types that something already produces and consumes exist; the rest of
// §6 (stop, apply, diff, input, checkpoint, promote, doctor) join with it.
type WorkerCommandType string

// Worker command types.
const (
	// CommandAssign offers a Run to the Worker under a lease (§10.4).
	CommandAssign WorkerCommandType = "assign"
)
