// Package execwrap is the process that runs a harness inside the Runtime
// (`cf worker exec`, §6.4) and the files it leaves in runs/<run-id>/ so that
// the daemon can tell what happened without being alive when it did.
//
// Four files, each written atomically, in the order the wrapper lives:
//
//	launch.json   written by the daemon: what to run
//	wrapper.json  the wrapper exists: its pid, group and start (before anything is spawned)
//	started.json  the harness was spawned
//	exit.json     it ended and nothing of its group is left
//
// output.log is the output, appended as it comes. With secrets it never holds
// their values: in headless mode the wrapper redacts the output on its way to
// the log and the pane, and in interactive mode the capture filter of the
// Runtime (RedactPipe) redacts what the terminal shows on its way to the log.
// What the terminal itself shows, its scrollback included, is not redacted.
package execwrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// File names in a run directory.
const (
	LaunchFile  = "launch.json"
	WrapperFile = "wrapper.json"
	StartedFile = "started.json"
	ExitFile    = "exit.json"
	OutputLog   = "output.log"
)

// Modes of execution (§7.4).
const (
	ModeHeadless    = "headless"
	ModeInteractive = "interactive"
)

// Launch is what the wrapper runs. Grace is how long after the harness is
// asked to stop it gets before it is killed (stop.grace). Gate, if set, is a
// file the Runtime creates once it has wired the terminal, and the wrapper
// waits for it before it starts anything. SecretEnv names the refs of the
// variables whose values the wrapper resolves just before it starts the
// harness: their values are never in this file.
type Launch struct {
	Argv      []string      `json:"argv"`
	Env       []string      `json:"env,omitempty"`
	SecretEnv []SecretEnv   `json:"secret_env,omitempty"`
	Dir       string        `json:"dir"`
	Mode      string        `json:"mode"`
	Grace     time.Duration `json:"grace_ns"`
	Gate      string        `json:"gate,omitempty"`
}

// SecretEnv is a variable of the harness whose value is the secret Ref names.
type SecretEnv struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// Wrapper says that the wrapper exists. Because the wrapper leads its group,
// PGID is also the group of everything it starts, known before the harness
// exists. Start is the procident token of the wrapper.
type Wrapper struct {
	PID       int       `json:"pid"`
	PGID      int       `json:"pgid"`
	Start     string    `json:"start_time"`
	StartedAt time.Time `json:"started_at"`
}

// Started says that the harness was spawned.
type Started struct {
	PID        int       `json:"pid"`
	PGID       int       `json:"pgid"`
	WrapperPID int       `json:"wrapper_pid"`
	Argv       []string  `json:"argv"`
	StartedAt  time.Time `json:"started_at"`
}

// Causes of an exit.
const (
	CauseExit = "exit"
	CauseStop = "stop"
)

// Exit says that the harness is over and that no process of the group is left
// but the wrapper. ExitCode is nil when a signal ended the harness or it never
// ran; Error says why it never ran, or why the wrapper could not do its job.
type Exit struct {
	ExitCode       *int      `json:"exit_code"`
	Signal         string    `json:"signal,omitempty"`
	ExitedAt       time.Time `json:"exited_at"`
	Cause          string    `json:"cause"`
	RemnantsKilled int       `json:"remnants_killed"` // processes of the group found after the harness exited
	// GroupKilled: some did not end within the grace, and the wrapper killed the
	// group as its last act, after writing this file.
	GroupKilled bool   `json:"group_killed,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Write stores v as the file in dir, atomically.
func Write(dir, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("execwrap: encoding %s: %w", name, err)
	}
	return atomicfile.Write(filepath.Join(dir, name), raw, 0o600)
}

// Read loads the file in dir into v. ok is false if the file does not exist.
func Read(dir, name string, v any) (ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // dir is the run directory
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("execwrap: reading %s: %w", name, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("execwrap: %s is corrupt: %w", name, err)
	}
	return true, nil
}
