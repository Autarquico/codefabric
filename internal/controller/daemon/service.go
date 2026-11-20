package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/support/migrate"
	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

// ServiceOptions parameterise InstallService and UninstallService.
type ServiceOptions struct {
	Paths config.Paths
	// Binary is the cflk the service runs; empty is this executable.
	Binary string
	// PATH is the PATH of the service; empty is one calculated now (svcunit.ServicePATH).
	PATH string
	// Manager is the service manager; zero (no GOOS) is that of this user.
	Manager svcunit.Env
	// Mover and Settle are where tests inject failures and the wait of the
	// migration from the old names (migrate.Service).
	Mover  migrate.Mover
	Settle func(ctx context.Context) error
	// LiveRuns is the check of the Runs of a Worker that is moved along with
	// the Controller (it shares its root); the composition of the binary gives
	// it, since the Controller does not know the Worker. nil refuses that move.
	LiveRuns migrate.LiveRuns
	// Home is the home directory of the user for the property lists of launchd; empty is this user's.
	Home string
}

// serviceSpec names the Controller's unit. Its KillMode is systemd's own: the
// Controller hosts no Run (invariant 3), and what it starts (git http-backend)
// goes with it.
func serviceSpec() svcunit.Spec {
	return svcunit.Spec{Name: "codeflock-controller", Label: "com.codeflock.controller"}
}

// oldServiceSpec is the unit as it was named before the rename.
func oldServiceSpec() svcunit.Spec {
	return svcunit.Spec{Name: "cf-controller", Label: "com.codefabric.controller"}
}

// InstallService installs the Controller of this machine as a systemd --user
// unit and leaves it running. Installing what is installed and running changes
// nothing.
func InstallService(ctx context.Context, o ServiceOptions) (svcunit.Result, error) {
	if err := o.launchdPlan(); err != nil {
		return svcunit.Result{}, err
	}
	env, _, _, err := controllerService(o)
	if err != nil {
		return svcunit.Result{}, err
	}
	if _, err := config.LoadController(o.Paths); err != nil {
		return svcunit.Result{}, err
	}
	res, migrated, err := o.migrateOldService(ctx, env)
	if migrated || err != nil {
		return res, err
	}
	if o.Paths.ConfigDir, o.Paths.DataDir, err = o.Mover.Adopt(ctx, env, o.Paths.ConfigDir, o.Paths.DataDir, nil); err != nil {
		return svcunit.Result{}, err
	}
	_, spec, warnings, err := controllerService(o)
	if err != nil {
		return svcunit.Result{}, err
	}
	res, err = svcunit.Install(ctx, env, spec)
	res.Warnings = append(warnings, res.Warnings...)
	return res, err
}

// migrateOldService moves the unit of the old name, cf-controller, to the new
// one, with the directories it uses; migrated is false if there is none.
func (o ServiceOptions) migrateOldService(ctx context.Context, env svcunit.Env) (res svcunit.Result, migrated bool, err error) {
	var warnings []string
	res, err = migrate.Service{
		Env: env, Old: oldServiceSpec(), Mover: o.Mover, Settle: o.Settle, Binary: o.Binary, LiveRuns: o.LiveRuns, Data: o.Paths.DataDir,
		New: func(_ migrate.Snapshot, dirs migrate.Dirs) (svcunit.Spec, error) {
			n := o
			n.Paths = config.Paths{ConfigDir: dirs.NewConfig, DataDir: dirs.NewData}
			_, spec, w, err := controllerService(n)
			warnings = w
			return spec, err
		},
	}.Migrate(ctx)
	switch {
	case errors.Is(err, migrate.ErrNoOldService):
		return svcunit.Result{}, false, nil
	case err != nil:
		return svcunit.Result{}, true, inputError(err)
	}
	res.Warnings = append(warnings, res.Warnings...)
	return res, true, nil
}

// launchdPlan is an error that says how to move by hand a Controller that
// runs under launchd, if there is one: cflk does not install the Controller
// there, so it does not move it either.
func (o ServiceOptions) launchdPlan() error {
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if cmp.Or(o.Manager.GOOS, runtime.GOOS) != "darwin" {
		return nil
	}
	for _, u := range config.OldUnits(home, "darwin") {
		if u.Kind != "controller" {
			continue
		}
		content, err := os.ReadFile(u.Path) //nolint:gosec // the property list of the old Controller
		if err != nil {
			return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s cannot be read, so the Controller was not moved: %v", u.Path, err), nil)
		}
		binary, err := migrate.PlanBinary(o.Binary, u.System)
		if err != nil {
			return inputError(err)
		}
		commands, err := migrate.LaunchPlan{Plist: u.Path, Content: content, System: u.System, Binary: binary, UserHome: home}.Commands()
		if err != nil {
			return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s cannot be moved: %v", u.Path, err), nil)
		}
		return domain.NewError(domain.CodeNotImplemented, fmt.Sprintf("the Controller runs under launchd (%s), which cflk does not manage: "+
			"nothing was changed. To move it, run, in order:\n\n%s", u.Path, strings.Join(commands, "\n")), nil)
	}
	return nil
}

// RenderService is the file InstallService would write, where, and what is
// worth a warning in it.
func RenderService(o ServiceOptions) (path string, content []byte, warnings []string, err error) {
	env, spec, warnings, err := controllerService(o)
	if err != nil {
		return "", nil, nil, err
	}
	path, content, err = svcunit.Render(env, spec)
	return path, content, warnings, err
}

// UninstallService stops the Controller's unit, disables it and removes it.
func UninstallService(ctx context.Context, o ServiceOptions) (svcunit.Result, error) {
	env, err := controllerManager(o.Manager)
	if err != nil {
		return svcunit.Result{}, err
	}
	return svcunit.Uninstall(ctx, env, serviceSpec())
}

func controllerService(o ServiceOptions) (svcunit.Env, svcunit.Spec, []string, error) {
	env, err := controllerManager(o.Manager)
	if err != nil {
		return svcunit.Env{}, svcunit.Spec{}, nil, err
	}
	binary, warning, err := svcunit.Program(o.Binary)
	if err != nil {
		return svcunit.Env{}, svcunit.Spec{}, nil, inputError(err)
	}
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}
	// The relay runs git http-backend.
	path, err := svcunit.ResolvePATH(env.GOOS, o.PATH, binary, "git")
	if err != nil {
		return svcunit.Env{}, svcunit.Spec{}, nil, inputError(err)
	}
	spec := serviceSpec()
	spec.Description, spec.Installer = "Code Flock Controller", "cflk controller install-service"
	spec.Argv = []string{binary, "controller", "run"}
	spec.Env = []svcunit.KV{{Key: "CFLK_CONFIG_DIR", Value: o.Paths.ConfigDir}, {Key: "CFLK_DATA_DIR", Value: o.Paths.DataDir}, {Key: "PATH", Value: path}}
	return env, spec, warnings, nil
}

// controllerManager is the manager of the Controller's service: systemd only, for now.
func controllerManager(env svcunit.Env) (svcunit.Env, error) {
	if env.GOOS == "" {
		host, err := svcunit.HostEnv()
		if err != nil {
			return svcunit.Env{}, err
		}
		env = host
	}
	if env.GOOS != "linux" {
		return svcunit.Env{}, domain.NewError(domain.CodeNotImplemented,
			"the Controller installs as a systemd --user service, on Linux only; elsewhere run `cflk controller run` under a supervisor of your own", nil)
	}
	return env, nil
}

// inputError makes what the caller must fix an invalid request.
func inputError(err error) error {
	var input *svcunit.InputError
	if errors.As(err, &input) {
		return domain.NewError(domain.CodeInvalidRequest, input.Error(), nil)
	}
	return err
}
