package domain

// Capacity is an amount of compute: what a Worker contributes, what is
// reserved on it, what a Run asks for.
type Capacity struct {
	CPU      int
	MemoryMB int
	DiskMB   int
}

// Placement is what the scheduler reads of a Run's frozen spec: where the Run
// may go and what it reserves there. Disk is 0 until Tasks can ask for it.
type Placement struct {
	Harness      string
	Capabilities []string
	Resources    Capacity
	Prefer       []string // Worker names, in order of preference
	Avoid        []string
	Require      []string
}
