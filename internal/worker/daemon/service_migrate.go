package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/support/migrate"
	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

// migrateOldService moves the service of the old name, cf-worker, to the new
// one, with the directories it uses; migrated is false if there is none. It
// changes nothing while a Run is live, and a failed step puts the old service
// back (migrate.Service).
func (o ServiceOptions) migrateOldService(ctx context.Context, env svcunit.Env) (res ServiceResult, migrated bool, err error) {
	old := oldServiceNames(o.Suffix)
	if env.GOOS == "darwin" {
		if err := o.launchDaemonPlan(old); err != nil {
			return ServiceResult{}, true, err
		}
	}
	var warnings []string
	r, err := migrate.Service{
		Env: env, Old: old, Mover: o.Mover, Settle: o.Settle, Binary: o.Binary, LiveRuns: NoLiveRuns(o.Sessions), Data: o.Paths.DataDir,
		New: func(was migrate.Snapshot, dirs migrate.Dirs) (svcunit.Spec, error) {
			spec, w, err := workerSpec(o.carryingOver(was, dirs), env.GOOS)
			warnings = w
			return spec, serviceError(err)
		},
	}.Migrate(ctx)
	switch {
	case errors.Is(err, migrate.ErrNoOldService):
		return ServiceResult{}, false, nil
	case err != nil:
		return ServiceResult{}, true, serviceError(err)
	}
	r.Warnings = append(warnings, r.Warnings...)
	return ServiceResult{Result: r}, true, nil
}

// carryingOver is o with what the old service had: the same arguments, the
// same TMUX_TMPDIR (it decides the tmux server of the Runs), the same PATH
// unless o gives one, and what else was in its environment.
func (o ServiceOptions) carryingOver(was migrate.Snapshot, dirs migrate.Dirs) ServiceOptions {
	o.Paths = config.Paths{ConfigDir: dirs.NewConfig, DataDir: dirs.NewData}
	o.TmuxTmpDir = was.Env["TMUX_TMPDIR"]
	if o.PATH == "" {
		o.PATH = was.Env["PATH"]
	}
	carried := migrate.CarriedEnv(was.Env, "PATH", "TMUX_TMPDIR")
	// What the caller gives wins over what the old unit had: no key twice.
	carried = slices.DeleteFunc(carried, func(kv svcunit.KV) bool {
		return slices.ContainsFunc(o.ExtraEnv, func(e svcunit.KV) bool { return e.Key == kv.Key })
	})
	o.ExtraEnv = append(carried, o.ExtraEnv...)
	args := was.Argv
	if len(args) >= 3 && args[1] == "worker" && args[2] == "run" {
		args = args[3:]
	} else {
		args = nil
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		flag, value, joined := strings.Cut(args[i], "=")
		target := map[string]*string{"--ssh-target": &o.SSHTarget, "--controller": &o.Controller}[flag]
		switch {
		case target == nil:
			rest = append(rest, args[i])
		case joined:
			setIfEmpty(target, value)
		case i+1 < len(args):
			i++
			setIfEmpty(target, args[i])
		}
	}
	o.ExtraArgs = append(rest, o.ExtraArgs...)
	return o
}

func setIfEmpty(target *string, value string) {
	if *target == "" {
		*target = value
	}
}

// launchDaemonPlan is an error that says how to move by hand the Worker that
// runs as a LaunchDaemon, if there is one: it is loaded by root, so this
// command, run as anyone, changes nothing.
func (o ServiceOptions) launchDaemonPlan(old svcunit.Spec) error {
	dir := o.LaunchDaemons
	if dir == "" {
		dir = "/Library/LaunchDaemons"
	}
	path := filepath.Join(dir, old.Label+".plist")
	content, err := os.ReadFile(path) //nolint:gosec // the property list of the old Worker
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s cannot be read, so the Worker was not moved: %v", path, err), nil)
	}
	plan := o.PlanBinary
	if plan == nil {
		plan = migrate.PlanBinary
	}
	binary, err := plan(o.Binary, true)
	if err != nil {
		return serviceError(err)
	}
	commands, err := migrate.LaunchPlan{Plist: path, Content: content, System: true, Binary: binary}.Commands()
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s cannot be moved: %v", path, err), nil)
	}
	return domain.NewError(domain.CodeNotImplemented, fmt.Sprintf("the Worker runs as a LaunchDaemon (%s), which only root can change: "+
		"nothing was changed. To move it, run, in order:\n\n%s", path, strings.Join(commands, "\n")), nil)
}
