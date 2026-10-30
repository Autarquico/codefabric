// Package envwrap is the supervisor of the Environment of a Run (`cf worker
// exec env`, §25): it prepares the worktree, runs the services the Run needs
// while it lives and stops them. It is modelled on the wrapper of the harness
// (package execwrap) and outlives the daemon the same way: it leads its process
// group, says who it is before it runs anything, waits for the daemon's
// authorization, and leaves its progress in files of the run directory.
//
// The files, in the order the supervisor lives:
//
//	env-launch.json      written by the daemon: what to run
//	env-wrapper.json     the supervisor exists (pid, group, start): before anything runs;
//	                     the first supervisor to claim it holds it, any other ends silently
//	env-authorized.json  written by the daemon once it recorded the identity: the gate,
//	                     bound to that identity and to the Run
//	prepared.json        prepare ended: ok, or failed with a redacted message
//	services-ready.json  every service is ready, or one failed and the started ones were cleaned
//	env-stop.json        written by the daemon: stop the services and end
//	env-exit.json        the services are stopped and nothing of the group is left but the supervisor
//
// environment.log is the output of every script, redacted of the Run's secrets.
// Everything is written atomically with execwrap.Write and read with execwrap.Read.
package envwrap

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// File names in a run directory.
const (
	LaunchFile     = "env-launch.json"
	WrapperFile    = "env-wrapper.json"
	AuthorizedFile = "env-authorized.json"
	PreparedFile   = "prepared.json"
	ServicesFile   = "services-ready.json"
	StopFile       = "env-stop.json"
	ExitFile       = "env-exit.json"
	OutputLog      = "environment.log"
)

// Default timeouts (all on the clock of the supervisor).
const (
	DefaultPrepareTimeout = 10 * time.Minute
	DefaultReadyTimeout   = 2 * time.Minute
	DefaultStopTimeout    = 30 * time.Second
	DefaultGrace          = 30 * time.Second
)

// MaxMessage bounds the messages that travel in the markers.
const MaxMessage = 2 << 10

// Launch is what the supervisor runs. Its scripts run with /bin/sh -c in Dir,
// in the environment of the Worker (without tmux) plus the secrets SecretEnv
// names, which the supervisor resolves itself and which are never in this file.
type Launch struct {
	// RunID and LeaseEpoch are the Run the supervisor is for: the authorization
	// names them, and a supervisor never runs under one that names another.
	RunID      string `json:"run_id"`
	LeaseEpoch uint64 `json:"lease_epoch"`

	Dir       string                      `json:"dir"`
	Prepare   string                      `json:"prepare"`
	Services  []domain.EnvironmentService `json:"services"`
	SecretEnv []execwrap.SecretEnv        `json:"secret_env,omitempty"`
	// RedactOutput redacts the output of the secrets the Run retains even when
	// it has no SecretEnv.
	RedactOutput bool `json:"redact_output,omitempty"`

	// Zero is the Default of each.
	PrepareTimeout time.Duration `json:"prepare_timeout_ns,omitempty"`
	ReadyTimeout   time.Duration `json:"ready_timeout_ns,omitempty"`
	StopTimeout    time.Duration `json:"stop_timeout_ns,omitempty"`
	Grace          time.Duration `json:"grace_ns,omitempty"`
}

// Authorized is the gate: the daemon wrote it after recording the identity of
// the supervisor, and nothing runs before it exists. It names the one process it
// authorizes (PID, PGID and Start, as in env-wrapper.json) and the Run it is for:
// any other supervisor that finds it ends without running anything.
type Authorized struct {
	RunID      string    `json:"run_id"`
	LeaseEpoch uint64    `json:"lease_epoch"`
	PID        int       `json:"pid"`
	PGID       int       `json:"pgid"`
	Start      string    `json:"start"`
	At         time.Time `json:"at"`
}

// AuthorizesWrapper says whether a authorizes the supervisor w of the Run
// (runID, leaseEpoch).
func (a Authorized) AuthorizesWrapper(runID string, leaseEpoch uint64, w execwrap.Wrapper) bool {
	return a.RunID == runID && a.LeaseEpoch == leaseEpoch && a.PID == w.PID && a.PGID == w.PGID && a.Start == w.Start && a.Start != ""
}

// Statuses of Prepared and Services.
const (
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// Prepared says how prepare ended. Once it exists prepare never runs again,
// not after a restart of the supervisor either. Message is redacted and
// bounded by MaxMessage; it is empty when Status is ok.
type Prepared struct {
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// Services says that every service is ready (Ready, in start order), or that
// one failed (Message, redacted and bounded) and the ones started were stopped.
type Services struct {
	Status  string    `json:"status"`
	Ready   []string  `json:"ready,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// StopOrder is the daemon asking the supervisor to stop the services and end.
type StopOrder struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// Causes of an Exit.
const (
	CauseStop           = "stop"
	CausePrepareFailed  = "prepare-failed"
	CauseServicesFailed = "services-failed"
	CauseError          = "error" // the supervisor could not do its job; Error says why
)

// StopFailure is a service whose stop command failed. Stopping is best effort
// (§25): the Run is released all the same, with a warning.
type StopFailure struct {
	Service string `json:"service"`
	Error   string `json:"error"`
}

// Exit says the supervisor is done and no process of its group is left but
// itself. GroupKilled: some did not end within the grace and the supervisor
// killed the group as its last act, after writing this file.
type Exit struct {
	Cause          string        `json:"cause"`
	ExitedAt       time.Time     `json:"exited_at"`
	StopFailures   []StopFailure `json:"stop_failures,omitempty"`
	RemnantsKilled int           `json:"remnants_killed"`
	GroupKilled    bool          `json:"group_killed,omitempty"`
	Error          string        `json:"error,omitempty"`
}
