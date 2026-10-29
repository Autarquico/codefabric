package domain

// Ownership says how CodeFabric treats a Workflow component (§8.3).
type Ownership string

// Ownership states.
const (
	OwnershipManaged Ownership = "managed"
	OwnershipLocal   Ownership = "local"
	OwnershipMerged  Ownership = "merged"
	OwnershipIgnored Ownership = "ignored"
)

// Workflow is a versioned composition of native harness configuration (§3.6).
type Workflow struct {
	ID           ID
	Name         string
	Version      int
	Digest       string
	ParentDigest string
}
