// Package doctor is the Worker's own verdict on its machine (§9.4): one
// ComponentHealth per thing a Run needs, from the runtime to the login of each
// harness. Every dependency is injected, so a check is a function of what the
// machine looks like and the package never reads a secret: it only asks
// whether one is there.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/globalapply"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

// ClaudeKeychainItem is where Claude Code keeps its login on macOS.
const ClaudeKeychainItem = "Claude Code-credentials"

// loginCommands is what a person runs, on the Worker, to log in to a harness.
var loginCommands = map[string]string{claude.ID: "claude login", "codex": "codex login"}

// globalAuth is a harness that keeps its login in a file of the host.
type globalAuth interface {
	GlobalAuth(host harness.Host) string
}

// Options parameterise New.
type Options struct {
	Host     harness.Host
	Adapters []harness.Adapter
	// Worker names the machine in the actions ("claude login en <worker>").
	Worker string
	// DataDir is the data directory of the Worker: its disk is the one measured,
	// and its baselines say what was applied.
	DataDir string
	Secrets secrets.Store
	MinFree Threshold // zero is DefaultMinFree
	// Git is the git binary; empty is the one in the PATH.
	Git string

	// Runtime checks the session runtime; nil checks tmux.
	Runtime func(ctx context.Context) error
	// RuntimeName names the runtime in the verdict; empty is tmux.
	RuntimeName string
	// Disk measures the filesystem of dir; nil is sysinfo.DiskOf.
	Disk func(dir string) (sysinfo.Disk, error)
	// Applied says what was applied to a harness; nil reads the baseline.
	Applied func(ctx context.Context, a harness.Adapter) ([]globalapply.Applied, error)
	// GOOS is the platform, for the checks that depend on it; empty is this one.
	GOOS string
	// RuntimeIsolated says the runtime confines the Runs (a container, a VM).
	// The zero value is the host itself, which is what tmux and a process are.
	RuntimeIsolated bool
}

// Doctor runs the checks. It remembers whether the disk was failing, which is
// all the state of the hysteresis.
type Doctor struct {
	o Options

	mu          sync.Mutex
	diskFailing bool
}

// New builds a Doctor over o.
func New(o Options) *Doctor {
	if o.MinFree == (Threshold{}) {
		o.MinFree, _ = ParseThreshold(DefaultMinFree)
	}
	if o.RuntimeName == "" {
		o.RuntimeName = "tmux"
	}
	if o.Runtime == nil {
		o.Runtime = func(ctx context.Context) error {
			t, err := tmux.New(tmux.Options{Socket: tmux.SocketName(o.DataDir)})
			if err != nil {
				return err
			}
			return t.Check(ctx)
		}
	}
	if o.Disk == nil {
		o.Disk = sysinfo.DiskOf
	}
	if o.Applied == nil {
		o.Applied = func(ctx context.Context, a harness.Adapter) ([]globalapply.Applied, error) {
			return (&globalapply.Engine{Adapter: a, Host: o.Host, DataDir: o.DataDir}).Applied(ctx)
		}
	}
	if o.Git == "" {
		o.Git = "git"
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	return &Doctor{o: o}
}

// Check runs every check; what it returns is fit for a heartbeat (Clean). The Controller decides "version" and the heartbeat
// itself says "controller": neither is here.
func (d *Doctor) Check(ctx context.Context) []v1.ComponentHealth {
	out := []v1.ComponentHealth{d.runtime(ctx), d.git(ctx), d.disk()}
	for _, a := range d.o.Adapters {
		out = append(out, d.harness(ctx, a)...)
	}
	for i := range out {
		out[i] = CleanComponent(out[i])
	}
	return out
}

// Health is the summary of components for a heartbeat: degraded if any fails.
func Health(components []v1.ComponentHealth) v1.Health {
	h := v1.Health{Status: v1.HealthOK, Components: components}
	for _, c := range components {
		if c.Status == v1.ComponentFail {
			h.Status, h.Message = v1.HealthDegraded, Clean(c.Name+": "+c.Message)
			break
		}
	}
	return h
}

// Failing is the number of components that fail.
func Failing(components []v1.ComponentHealth) int {
	n := 0
	for _, c := range components {
		if c.Status == v1.ComponentFail {
			n++
		}
	}
	return n
}

func ok(name, msg string) v1.ComponentHealth {
	return v1.ComponentHealth{Name: name, Status: v1.ComponentOK, Message: msg}
}

func (d *Doctor) runtime(ctx context.Context) v1.ComponentHealth {
	if err := d.o.Runtime(ctx); err != nil {
		action := "revisa el runtime de sesiones en " + d.o.Worker
		if d.o.RuntimeName == "tmux" {
			action = "instala tmux 3.0 o posterior en " + d.o.Worker
		}
		return v1.ComponentHealth{
			Name: "runtime", Status: v1.ComponentFail, Message: err.Error(),
			Action: action,
		}
	}
	return ok("runtime", d.o.RuntimeName+" disponible")
}

func (d *Doctor) git(ctx context.Context) v1.ComponentHealth {
	res, err := d.o.Host.Run(ctx, harness.Cmd{Argv: []string{d.o.Git, "--version"}})
	if err != nil || res.ExitCode != 0 {
		return v1.ComponentHealth{
			Name: "git", Status: v1.ComponentFail, Message: "git no está disponible",
			Action: "instala git en " + d.o.Worker,
		}
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\n")
	return ok("git", version)
}

func (d *Doctor) disk() v1.ComponentHealth {
	disk, err := d.o.Disk(d.o.DataDir)
	if err != nil {
		return v1.ComponentHealth{Name: "disk", Status: v1.ComponentWarn, Message: "no se pudo medir el disco: " + err.Error()}
	}
	d.mu.Lock()
	d.diskFailing = d.o.MinFree.Failing(disk.FreeBytes, disk.TotalBytes, d.diskFailing)
	failing := d.diskFailing
	d.mu.Unlock()

	pct := 0.0
	if disk.TotalBytes > 0 {
		pct = 100 * float64(disk.FreeBytes) / float64(disk.TotalBytes)
	}
	msg := fmt.Sprintf("%.0f%% libre (%s); mínimo %s", pct, humanBytes(disk.FreeBytes), d.o.MinFree)
	if failing {
		return v1.ComponentHealth{
			Name: "disk", Status: v1.ComponentFail, Message: msg,
			Action: "libera espacio en " + d.o.Worker + " o baja --disk-min-free",
		}
	}
	return ok("disk", msg)
}

// harness checks the harnesses this machine offers: one that is not installed
// is not offered, and has nothing to report.
func (d *Doctor) harness(ctx context.Context, a harness.Adapter) []v1.ComponentHealth {
	id := a.ID()
	det, err := a.Detect(ctx, d.o.Host)
	switch {
	case err != nil:
		return []v1.ComponentHealth{{
			Name: "harness:" + id, Status: v1.ComponentFail, Message: "no se pudo detectar: " + err.Error(),
			Action: "revisa la instalación de " + id + " en " + d.o.Worker,
		}}
	case !det.Installed:
		return nil
	}
	out := []v1.ComponentHealth{d.config(ctx, a)}
	if c, warn := d.isolation(a); warn {
		out = append(out, c)
	}
	if _, isAuth := a.(globalAuth); isAuth {
		out = append(out, d.auth(ctx, a))
	}
	if _, isInspector := a.(harness.GlobalInspector); isInspector {
		out = append(out, d.workflow(ctx, a))
	}
	return out
}

func (d *Doctor) config(ctx context.Context, a harness.Adapter) v1.ComponentHealth {
	name := "harness:" + a.ID()
	val, err := a.ValidateConfig(ctx, d.o.Host)
	switch {
	case errors.Is(err, harness.ErrUnsupported):
	case err != nil:
		return v1.ComponentHealth{
			Name: name, Status: v1.ComponentFail, Message: "no se pudo validar la configuración: " + err.Error(),
			Action: "revisa la configuración de " + a.ID() + " en " + d.o.Worker,
		}
	case len(val.Issues) > 0:
		// A harness with ConfigIsolation runs on the configuration materialised
		// for each Run, never on the host's: a problem there is only a warning.
		// Without it, a configuration that does not work takes no Runs.
		status := v1.ComponentWarn
		if val.Blocking() && !a.Capabilities().ConfigIsolation {
			status = v1.ComponentFail
		}
		issue := val.Issues[0]
		msg := issue.Message
		if issue.Component != "" {
			msg = issue.Component + ": " + msg
		}
		return v1.ComponentHealth{
			Name: name, Status: status, Message: fmt.Sprintf("configuración con %d problema(s): %s", len(val.Issues), msg),
			Action: "revisa la configuración de " + a.ID() + " en " + d.o.Worker,
		}
	}
	return ok(name, "instalado y con configuración válida")
}

// isolation warns of a harness that would run with its permission prompts
// skipped directly on the host: because it launches that way on a runtime that
// does not confine it, or because the host configuration it reads says so.
func (d *Doctor) isolation(a harness.Adapter) (v1.ComponentHealth, bool) {
	var reasons []string
	if a.Capabilities().PreapprovedPermissions && !d.o.RuntimeIsolated {
		reasons = append(reasons, a.ID()+" se ejecuta con permisos preaprobados directamente en el host (sin aislamiento)")
	}
	if d.hostConfigBypasses(a) {
		reasons = append(reasons, a.ID()+" no aísla su configuración y la del host salta los permisos (bypass)")
	}
	if len(reasons) == 0 {
		return v1.ComponentHealth{}, false
	}
	return v1.ComponentHealth{
		Name: "isolation:" + a.ID(), Status: v1.ComponentWarn,
		Message: strings.Join(reasons, "; "),
		Action:  "usa un Worker dedicado o un runtime aislado para " + a.ID() + " en " + d.o.Worker,
	}, true
}

func (d *Doctor) hostConfigBypasses(a harness.Adapter) bool {
	detector, ok := a.(harness.BypassDetector)
	if !ok || a.Capabilities().ConfigIsolation {
		return false
	}
	bypasses, err := detector.BypassesPermissions(d.o.Host)
	return err == nil && bypasses
}

// auth says whether a login is there, never what it is.
func (d *Doctor) auth(ctx context.Context, a harness.Adapter) v1.ComponentHealth {
	name := "auth:" + a.ID()
	if a.ID() == claude.ID && (d.hasSecret(ctx, claude.OAuthTokenRef) || d.hasKeychainLogin(ctx)) {
		return ok(name, "login de Claude Code presente")
	}
	if auth, isAuth := a.(globalAuth); isAuth {
		if _, err := os.Stat(auth.GlobalAuth(d.o.Host)); err == nil {
			return ok(name, "login presente")
		}
	}
	login := loginCommands[a.ID()]
	if login == "" {
		login = "inicia sesión en " + a.ID()
	}
	return v1.ComponentHealth{
		Name: name, Status: v1.ComponentFail, Message: "sin login de " + a.ID(),
		Action: login + " en " + d.o.Worker,
	}
}

func (d *Doctor) hasSecret(ctx context.Context, ref string) bool {
	if d.o.Secrets == nil {
		return false
	}
	list, err := d.o.Secrets.List(ctx) // metadata only: no value is read
	if err != nil {
		return false
	}
	for _, m := range list {
		if m.Ref == ref {
			return true
		}
	}
	return false
}

// hasKeychainLogin asks the macOS Keychain whether the item exists. Without -w
// or -g the command prints attributes and never the password; its output is
// dropped anyway.
func (d *Doctor) hasKeychainLogin(ctx context.Context) bool {
	if d.o.GOOS != "darwin" {
		return false
	}
	res, err := d.o.Host.Run(ctx, harness.Cmd{Argv: []string{"security", "find-generic-password", "-s", ClaudeKeychainItem}})
	return err == nil && res.ExitCode == 0
}

func (d *Doctor) workflow(ctx context.Context, a harness.Adapter) v1.ComponentHealth {
	name := "workflow:" + a.ID()
	applied, err := d.o.Applied(ctx, a)
	if err != nil {
		return v1.ComponentHealth{Name: name, Status: v1.ComponentWarn, Message: "no se pudo comprobar lo aplicado: " + err.Error()}
	}
	var drifted, missing []string
	managed := 0
	for _, c := range applied {
		if c.Ownership != harness.Managed {
			continue
		}
		managed++
		switch {
		case c.Missing:
			missing = append(missing, c.Name)
		case c.Drift:
			drifted = append(drifted, c.Name)
		}
	}
	if len(drifted)+len(missing) > 0 {
		return v1.ComponentHealth{
			Name: name, Status: v1.ComponentWarn,
			Message: describeDrift(drifted, missing),
			Action:  "cf workflow status --worker " + d.o.Worker,
		}
	}
	if managed == 0 {
		return ok(name, "ningún workflow aplicado")
	}
	return ok(name, fmt.Sprintf("%d componente(s) aplicados, sin cambios", managed))
}

func describeDrift(drifted, missing []string) string {
	var parts []string
	if len(drifted) > 0 {
		parts = append(parts, "editados en el host: "+strings.Join(drifted, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "aplicados pero ausentes: "+strings.Join(missing, ", "))
	}
	return strings.Join(parts, "; ")
}

// Every is how often the daemon runs the checks: not every heartbeat, since
// some of them start processes.
const Every = time.Minute
