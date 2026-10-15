// Package svcunittest has service managers that live in memory, for the tests
// of what installs services: they keep the state a real one would and record
// every command they are given.
package svcunittest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/svcunit"
)

// Program is an executable file only this user can write, for a service to
// run: svcunit.Program refuses one that is not there.
func Program(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cf")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a program, so executable
		t.Fatal(err)
	}
	return path
}

// recorder keeps the commands a fake was given and the failures it was told to give.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
	fail  map[string]error
}

// Calls are the commands given so far, each joined with spaces.
func (r *recorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, argv := range r.calls {
		out[i] = strings.Join(argv, " ")
	}
	return out
}

// Fail makes every command that starts with prefix fail with err; a nil err
// makes them work again.
func (r *recorder) Fail(prefix string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail == nil {
		r.fail = map[string]error{}
	}
	if err == nil {
		delete(r.fail, prefix)
		return
	}
	r.fail[prefix] = err
}

func (r *recorder) record(argv []string) error {
	r.calls = append(r.calls, argv)
	line := strings.Join(argv, " ")
	for prefix, err := range r.fail {
		if strings.HasPrefix(line, prefix) {
			return err
		}
	}
	return nil
}

// Exit is a tool that exited with code.
func Exit(argv []string, code int, stderr string) error {
	return &svcunit.CommandError{Argv: argv, Code: code, Stderr: stderr}
}

// Systemd is a user's systemd with one unit, the one at Path.
type Systemd struct {
	recorder
	Path string // where the unit file is: the one systemd loads once reloaded

	LoadState, UnitFileState, ActiveState string
	NeedDaemonReload                      bool
	Fragment                              string // FragmentPath; empty until a reload finds Path
	// DropInKillMode overrides, once reloaded, the KillMode of the file, as a drop-in would.
	DropInKillMode string
	// Elsewhere is a systemd that looks for units in another directory: a reload does not find Path.
	Elsewhere bool
	killMode  string
	Linger    string // what loginctl says; "" fails
}

// NewSystemd is a systemd that does not know the unit at path yet.
func NewSystemd(path string) *Systemd {
	return &Systemd{Path: path, LoadState: "not-found", UnitFileState: "", ActiveState: "inactive", Linger: "yes"}
}

// Loaded makes it as if the unit file at Path had been loaded, enabled or not and running or not.
func (f *Systemd) Loaded(enabled, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reload()
	f.UnitFileState, f.ActiveState = "disabled", "inactive"
	if enabled {
		f.UnitFileState = "enabled"
	}
	if active {
		f.ActiveState = "active"
	}
}

func (f *Systemd) reload() {
	f.NeedDaemonReload = false
	f.LoadState, f.Fragment, f.killMode = "not-found", "", ""
	file, err := os.ReadFile(f.Path)
	if err != nil || f.Elsewhere {
		return
	}
	f.LoadState, f.Fragment, f.killMode = "loaded", f.Path, "control-group"
	if strings.Contains(string(file), "\nKillMode=process\n") {
		f.killMode = "process"
	}
	if f.DropInKillMode != "" {
		f.killMode = f.DropInKillMode
	}
}

// Run is systemctl --user and loginctl.
func (f *Systemd) Run(_ context.Context, argv ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(argv); err != nil {
		return "", err
	}
	if argv[0] == "loginctl" {
		if f.Linger == "" {
			return "", Exit(argv, 1, "Failed to connect to bus")
		}
		return f.Linger + "\n", nil
	}
	if len(argv) < 3 || argv[0] != "systemctl" || argv[1] != "--user" {
		return "", fmt.Errorf("svcunittest: unexpected command %q", argv)
	}
	switch argv[2] {
	case "show":
		yes := map[bool]string{true: "yes", false: "no"}[f.NeedDaemonReload]
		return fmt.Sprintf("LoadState=%s\nUnitFileState=%s\nActiveState=%s\nNeedDaemonReload=%s\nFragmentPath=%s\nKillMode=%s\nDropInPaths=\n",
			f.LoadState, f.UnitFileState, f.ActiveState, yes, f.Fragment, f.killMode), nil
	case "daemon-reload":
		f.reload()
	case "enable":
		// systemd 255 says every unit needs a reload after a change to the unit files.
		f.UnitFileState, f.NeedDaemonReload = "enabled", true
	case "disable":
		f.UnitFileState, f.NeedDaemonReload = "disabled", true
	case "start", "restart":
		if f.LoadState != "loaded" {
			return "", Exit(argv, 5, "Unit not found.")
		}
		f.ActiveState = "active"
	case "stop", "reset-failed":
		f.ActiveState = "inactive"
	default:
		return "", fmt.Errorf("svcunittest: unexpected command %q", argv)
	}
	return "", nil
}

// Launchd is a user's launchd with one agent, the one at Path.
type Launchd struct {
	recorder
	Path, Label string
	UID         int

	GUI      bool            // the user has a graphical session
	Domain   string          // where the agent is loaded; "" is nowhere
	Twin     bool            // the agent is loaded in the other domain too
	Running  bool            // the agent has a process
	Disabled map[string]bool // by domain
	// Abandon is what print says of AbandonProcessGroup once the agent is
	// loaded: "" reads it from the file, "unknown" leaves it out.
	Abandon string
	from    string // the file the agent was loaded from
}

// NewLaunchd is a launchd of a user with a graphical session, with nothing loaded.
func NewLaunchd(path, label string, uid int) *Launchd {
	return &Launchd{Path: path, Label: label, UID: uid, GUI: true, Disabled: map[string]bool{}}
}

// GUIDomain is the domain of the user's graphical session.
func (f *Launchd) GUIDomain() string { return fmt.Sprintf("gui/%d", f.UID) }

// UserDomain is the user's background domain.
func (f *Launchd) UserDomain() string { return fmt.Sprintf("user/%d", f.UID) }

// Load makes it as if the file at Path had been loaded in domain.
func (f *Launchd) Load(domain string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Domain, f.Running, f.from = domain, running, f.Path
}

// Run is launchctl.
func (f *Launchd) Run(_ context.Context, argv ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(argv); err != nil {
		return "", err
	}
	if len(argv) < 3 || argv[0] != "launchctl" {
		return "", fmt.Errorf("svcunittest: unexpected command %q", argv)
	}
	target := argv[2]
	switch argv[1] {
	case "print":
		return f.print(argv, target)
	case "print-disabled":
		var out strings.Builder
		out.WriteString("disabled services = {\n")
		if off, known := f.Disabled[target]; known {
			fmt.Fprintf(&out, "\t\t%q => %s\n", f.Label, map[bool]string{true: "disabled", false: "enabled"}[off])
		}
		out.WriteString("}\n")
		return out.String(), nil
	case "enable":
		f.Disabled[strings.TrimSuffix(target, "/"+f.Label)] = false
	case "bootstrap":
		if f.Disabled[target] {
			return "", Exit(argv, 5, "Bootstrap failed: 5: Input/output error")
		}
		if f.Domain != "" {
			return "", Exit(argv, 5, "Bootstrap failed: 5: Input/output error")
		}
		f.Domain, f.Running, f.from = target, true, argv[3]
	case "bootout":
		if target != f.Domain+"/"+f.Label {
			return "", Exit(argv, 3, "Boot-out failed: 3: No such process")
		}
		f.Domain, f.Running = "", false
	case "kickstart":
		if slices.Contains(argv, "-k") {
			return "", fmt.Errorf("svcunittest: kickstart -k was not expected")
		}
		f.Running = true
	default:
		return "", fmt.Errorf("svcunittest: unexpected command %q", argv)
	}
	return "", nil
}

func (f *Launchd) print(argv []string, target string) (string, error) {
	if target == f.GUIDomain() {
		if f.GUI {
			return "gui domain\n", nil
		}
		return "", Exit(argv, 112, "Could not find domain for user gui")
	}
	if target == f.UserDomain() {
		return "user domain\n", nil
	}
	domain, _ := strings.CutSuffix(target, "/"+f.Label)
	if domain == f.GUIDomain() && !f.GUI {
		return "", Exit(argv, 112, "Could not find domain for user gui")
	}
	loaded := f.Domain != "" && (domain == f.Domain || f.Twin && slices.Contains([]string{f.GUIDomain(), f.UserDomain()}, domain))
	if !loaded {
		return "", Exit(argv, 113, fmt.Sprintf("Could not find service %q in domain", f.Label))
	}
	state := "not running"
	if f.Running {
		state = "running"
	}
	out := fmt.Sprintf("%s = {\n\tpath = %s\n\ttype = LaunchAgent\n\tstate = %s\n\n\tresource coalition = {\n\t\tstate = active\n\t}\n", target, f.from, state)
	switch f.Abandon {
	case "unknown":
	case "":
		file, _ := os.ReadFile(f.from)
		props := "runatload | inferred program"
		if strings.Contains(string(file), "<key>AbandonProcessGroup</key>") {
			props = "runatload | abandon process group | inferred program"
		}
		out += "\tproperties = " + props + "\n"
	default:
		out += "\tproperties = " + f.Abandon + "\n"
	}
	return out + "}\n", nil
}
