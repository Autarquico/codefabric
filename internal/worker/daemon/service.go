package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/svcunit"
)

// ServiceOptions parameterise InstallService and UninstallService.
type ServiceOptions struct {
	Paths config.Paths
	// Binary is the cf the service runs; empty is this executable.
	Binary string
	// PATH is the PATH of the service; empty is one calculated now (svcunit.ServicePATH).
	PATH string
	// TmuxTmpDir, if set, is the TMUX_TMPDIR of the service, so that it runs
	// its Runs in the tmux server that clients of this machine find.
	TmuxTmpDir string
	SSHTarget  string // as for `cf worker run`
	Controller string // as for `cf worker run`
	// Manager is the service manager; zero (no GOOS) is that of this user.
	Manager svcunit.Env
	// Sessions lists the sessions of Runs in the tmux server of the Worker with
	// a data directory; nil asks tmux.
	Sessions func(ctx context.Context, dataDir, tmuxTmpDir string) ([]string, error)

	// For tests: a suffix of the unit and of the label, and more environment
	// and more arguments for `cf worker run`.
	Suffix    string
	ExtraEnv  []svcunit.KV
	ExtraArgs []string
}

// ServiceResult is what InstallService or UninstallService did, with the Runs
// that an uninstall left unsupervised.
type ServiceResult struct {
	svcunit.Result
	ActiveRuns []string `json:"active_runs,omitempty"`
}

// InstallService installs the Worker of this machine as a service of the
// user's manager (systemd --user on Linux, launchd on macOS) and leaves it
// running. The service keeps tmux, and so the Runs, alive across its own
// restarts (§6.4). Installing what is installed and running changes nothing.
func InstallService(ctx context.Context, o ServiceOptions) (ServiceResult, error) {
	env, err := managerEnv(o.Manager)
	if err != nil {
		return ServiceResult{}, err
	}
	spec, warnings, err := workerSpec(o, env.GOOS)
	if err != nil {
		return ServiceResult{}, serviceError(err)
	}
	if exists, err := client.CredentialsExist(o.Paths.WorkerCredentialsDir()); err != nil {
		return ServiceResult{}, err
	} else if !exists {
		// The service would only fail and be restarted, over and over.
		return ServiceResult{}, domain.NewError(domain.CodeUnauthenticated, "this machine has not joined as a Worker; run `cf worker join <invitation>` first", nil)
	}
	if installed, found, err := svcunit.InstalledEnv(env, spec); err == nil && found {
		warnings = append(warnings, tmuxServerMoved(installed, o)...)
	}
	res, err := svcunit.Install(ctx, env, spec)
	res.Warnings = append(warnings, res.Warnings...)
	return ServiceResult{Result: res}, serviceError(err)
}

// tmuxServerMoved warns of what decides the tmux server of the Runs, the data
// directory and TMUX_TMPDIR, changing from the installed service to this one:
// the new Worker does not see the Runs of the old server.
func tmuxServerMoved(installed map[string]string, o ServiceOptions) []string {
	var warnings []string
	for _, v := range []struct{ what, was, is string }{
		{"the data directory", installed["CF_DATA_DIR"], o.Paths.DataDir},
		{"TMUX_TMPDIR", installed["TMUX_TMPDIR"], o.TmuxTmpDir},
	} {
		if v.was != v.is {
			warnings = append(warnings, fmt.Sprintf("the service used %s %q and now uses %q: "+
				"the Runs left in the tmux server of the old one are not adopted", v.what, v.was, v.is))
		}
	}
	return warnings
}

// RenderService is the file InstallService would write, where, and what is
// worth a warning in it.
func RenderService(o ServiceOptions) (path string, content []byte, warnings []string, err error) {
	env, err := managerEnv(o.Manager)
	if err != nil {
		return "", nil, nil, err
	}
	spec, warnings, err := workerSpec(o, env.GOOS)
	if err != nil {
		return "", nil, nil, serviceError(err)
	}
	path, content, err = svcunit.Render(env, spec)
	return path, content, warnings, serviceError(err)
}

// UninstallService stops and removes the service of the Worker. It never ends a
// Run: the Runs that are in tmux, listed from the server the installed service
// used, go on unsupervised, and the result says so.
func UninstallService(ctx context.Context, o ServiceOptions) (ServiceResult, error) {
	spec := serviceNames(o.Suffix)
	env, err := managerEnv(o.Manager)
	if err != nil {
		return ServiceResult{}, err
	}
	installed, found, readErr := svcunit.InstalledEnv(env, spec)
	var (
		runs    []string
		listErr error
	)
	switch {
	case readErr != nil:
		listErr = readErr
	case !found:
		listErr = errors.New("there is no installed file to read the data directory of the service from")
	case installed["CF_DATA_DIR"] == "":
		listErr = errors.New("the installed service does not say its data directory")
	default:
		sessions := o.Sessions
		if sessions == nil {
			sessions = tmuxSessions
		}
		runs, listErr = sessions(ctx, installed["CF_DATA_DIR"], installed["TMUX_TMPDIR"])
	}
	res, err := svcunit.Uninstall(ctx, env, spec)
	out := ServiceResult{Result: res}
	if err != nil || !found && !res.Changed {
		return out, serviceError(err)
	}
	unsupervised := "unsupervised until the Worker is back (`cf worker install-service` or `cf worker run`)"
	if res.Manager == "systemd" {
		unsupervised += "; without lingering, they may be lost when the user's last session ends"
	}
	switch {
	case listErr != nil:
		out.Warnings = append(out.Warnings, fmt.Sprintf("could not list the Runs still in tmux (%v): uninstalling stops none, "+
			"and any there are go on %s", listErr, unsupervised))
	case len(runs) > 0:
		out.ActiveRuns = runs
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d Runs (%s) go on in tmux, %s",
			len(runs), strings.Join(runs, ", "), unsupervised))
	}
	return out, nil
}

// serviceNames is the spec of the service with only what names it.
func serviceNames(suffix string) svcunit.Spec {
	return svcunit.Spec{Name: "cf-worker" + suffix, Label: "com.codefabric.worker" + suffix, KeepProcesses: true}
}

func workerSpec(o ServiceOptions, goos string) (svcunit.Spec, []string, error) {
	var warnings []string
	binary, warning, err := svcunit.Program(o.Binary)
	if err != nil {
		return svcunit.Spec{}, nil, err
	}
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if o.SSHTarget != "" && o.SSHTarget != NoSSHTarget && !domain.ValidSSHTarget(o.SSHTarget) {
		return svcunit.Spec{}, nil, domain.NewError(domain.CodeInvalidRequest, "--ssh-target must be [user@]host[:port], or none", nil)
	}
	// Relative, the socket would be found from where the service starts, not from here.
	if o.TmuxTmpDir != "" && !filepath.IsAbs(o.TmuxTmpDir) {
		return svcunit.Spec{}, nil, domain.NewError(domain.CodeInvalidRequest,
			fmt.Sprintf("TMUX_TMPDIR must be an absolute path to go into the service, and %q is not one", o.TmuxTmpDir), nil)
	}
	path, err := svcunit.ResolvePATH(goos, o.PATH, binary, "tmux", "git")
	if err != nil {
		return svcunit.Spec{}, nil, err
	}

	argv := []string{binary, "worker", "run"}
	if o.SSHTarget != "" {
		argv = append(argv, "--ssh-target", o.SSHTarget)
	}
	if o.Controller != "" {
		argv = append(argv, "--controller", o.Controller)
	}
	env := []svcunit.KV{{Key: "CF_CONFIG_DIR", Value: o.Paths.ConfigDir}, {Key: "CF_DATA_DIR", Value: o.Paths.DataDir}, {Key: "PATH", Value: path}}
	if o.TmuxTmpDir != "" {
		env = append(env, svcunit.KV{Key: "TMUX_TMPDIR", Value: o.TmuxTmpDir})
	}
	spec := serviceNames(o.Suffix)
	spec.Description, spec.Installer = "CodeFabric Worker", "cf worker install-service"
	spec.Argv, spec.Env = append(argv, o.ExtraArgs...), append(env, o.ExtraEnv...)
	return spec, warnings, nil
}

func managerEnv(env svcunit.Env) (svcunit.Env, error) {
	if env.GOOS != "" {
		return env, nil
	}
	return svcunit.HostEnv()
}

// serviceError makes an operating system with no manager, and what the caller
// must fix, errors of their own.
func serviceError(err error) error {
	var input *svcunit.InputError
	switch {
	case errors.Is(err, svcunit.ErrUnsupported):
		return domain.NewError(domain.CodeNotImplemented, "installing the Worker as a service needs systemd (Linux) or launchd (macOS)", nil)
	case errors.As(err, &input):
		return domain.NewError(domain.CodeInvalidRequest, input.Error(), nil)
	}
	return err
}

// tmuxSessions lists the Runs in the tmux server of the Worker of dataDir.
func tmuxSessions(ctx context.Context, dataDir, tmuxTmpDir string) ([]string, error) {
	rt, err := tmux.New(tmux.Options{
		Socket: tmux.SocketName(config.Paths{DataDir: dataDir}.WorkerDataDir()), TmpDir: tmuxTmpDir,
	})
	if err != nil {
		return nil, err
	}
	return rt.List(ctx)
}
