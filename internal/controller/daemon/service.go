package daemon

import (
	"context"
	"errors"

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
	// Manager is the service manager; zero (no GOOS) is that of this user.
	Manager svcunit.Env
}

// serviceSpec names the Controller's unit. Its KillMode is systemd's own: the
// Controller hosts no Run (invariant 3), and what it starts (git http-backend)
// goes with it.
func serviceSpec() svcunit.Spec {
	return svcunit.Spec{Name: "cf-controller", Label: "com.codefabric.controller"}
}

// InstallService installs the Controller of this machine as a systemd --user
// unit and leaves it running. Installing what is installed and running changes
// nothing.
func InstallService(ctx context.Context, o ServiceOptions) (svcunit.Result, error) {
	env, spec, warnings, err := controllerService(o)
	if err != nil {
		return svcunit.Result{}, err
	}
	if _, err := config.LoadController(o.Paths); err != nil {
		return svcunit.Result{}, err
	}
	res, err := svcunit.Install(ctx, env, spec)
	res.Warnings = append(warnings, res.Warnings...)
	return res, err
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
	spec.Description, spec.Installer = "CodeFabric Controller", "cf controller install-service"
	spec.Argv = []string{binary, "controller", "run"}
	spec.Env = []svcunit.KV{{Key: "CF_CONFIG_DIR", Value: o.Paths.ConfigDir}, {Key: "CF_DATA_DIR", Value: o.Paths.DataDir}, {Key: "PATH", Value: path}}
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
			"the Controller installs as a systemd --user service, on Linux only; elsewhere run `cf controller run` under a supervisor of your own", nil)
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
