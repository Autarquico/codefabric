package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
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
		Env: env, Old: old, Mover: o.Mover, Settle: o.Settle, Preflight: o.noLiveRuns,
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
	var extra []string
	for key := range was.Env {
		if !slices.Contains([]string{"CF_CONFIG_DIR", "CF_DATA_DIR", "PATH", "TMUX_TMPDIR"}, key) {
			extra = append(extra, key)
		}
	}
	slices.Sort(extra)
	var carried []svcunit.KV
	for _, key := range extra {
		carried = append(carried, svcunit.KV{Key: key, Value: was.Env[key]})
	}
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

// noLiveRuns refuses to touch the old service while a Run is live on it: by
// its database and by its tmux, the one that its data directory and TMUX_TMPDIR
// name. What cannot be checked counts as a Run.
func (o ServiceOptions) noLiveRuns(ctx context.Context, was migrate.Snapshot, dirs migrate.Dirs) error {
	old := config.Paths{DataDir: dirs.OldData}
	inDB, dbErr := liveRunsInDatabase(ctx, old.WorkerStateFile())
	sessions := o.Sessions
	if sessions == nil {
		sessions = tmuxSessions
	}
	inTmux, tmuxErr := sessions(ctx, tmux.Options{
		Socket:    tmux.SocketName(old.WorkerDataDir()),
		TmpDir:    was.Env["TMUX_TMPDIR"],
		OwnTmpDir: true,
	})
	if err := errors.Join(dbErr, tmuxErr); err != nil {
		return domain.NewError(domain.CodeWorkerBusy, fmt.Sprintf("could not tell whether Runs are live on this Worker (%v): "+
			"that counts as there being some, and nothing was changed; fix it, or drain the Worker with `cflk worker drain <worker>`", err), nil)
	}
	if inDB > 0 || len(inTmux) > 0 {
		return domain.NewError(domain.CodeWorkerBusy, fmt.Sprintf("%d Runs are live on this Worker (%d in its database, %d in tmux): "+
			"nothing was changed; drain it with `cflk worker drain <worker>`, wait for them to end, and run this again", max(inDB, len(inTmux)), inDB, len(inTmux)), nil)
	}
	return nil
}

// liveRunsInDatabase counts the assignments of the Worker that have not ended,
// reading the file as it is: the Worker may be running, and nothing is migrated.
func liveRunsInDatabase(ctx context.Context, path string) (n int, err error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+
		url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "query_only(1)"}}.Encode())
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_runs WHERE phase NOT IN ('rejected', 'discarded', 'stopped', 'aborted')`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return n, nil
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
	home, _ := os.UserHomeDir()
	commands, err := migrate.LaunchPlan{Plist: path, Content: content, System: true, Binary: o.Binary, UserHome: home}.Commands()
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s cannot be moved: %v", path, err), nil)
	}
	return domain.NewError(domain.CodeNotImplemented, fmt.Sprintf("the Worker runs as a LaunchDaemon (%s), which only root can change: "+
		"nothing was changed. To move it, run, in order:\n\n%s", path, strings.Join(commands, "\n")), nil)
}
