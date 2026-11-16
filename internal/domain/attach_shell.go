package domain

// AttachShellCapability is that of a Worker that opens a shell in the worktree
// of a Run when the Controller relays a request for one (live-attach spec). The
// Worker announces it only while its local setting allow_shell is on and its
// attach relay runs; the Controller does not grant a shell on a Worker without it.
const AttachShellCapability = "attach-shell:v1"

// Names of the two keys of the shell policy, as CF_POLICY_DENIED details say them.
const (
	PolicyProjectShell     = "project.shell"
	PolicyWorkerAllowShell = "worker.allow_shell"
)
