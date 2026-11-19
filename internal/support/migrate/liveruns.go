package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/worker/state"
)

// Runs is the check that no Run is live on a Worker, which any move of its
// data directory needs: the name of the tmux socket comes from that directory,
// so the Runs would not be found again.
type Runs struct {
	// Sessions lists the Runs in the tmux server o names; nil asks tmux.
	Sessions func(ctx context.Context, o tmux.Options) ([]string, error)
}

// None refuses while a Run is live on the Worker whose data directory is
// dataDir and whose tmux uses tmuxTmpDir: by its database and by its tmux.
// What cannot be checked counts as a Run.
func (r Runs) None(ctx context.Context, tmuxTmpDir, dataDir string) error {
	paths := config.Paths{DataDir: dataDir}
	inDB, dbErr := liveRunsInDatabase(ctx, paths.WorkerStateFile())
	sessions := r.Sessions
	if sessions == nil {
		sessions = tmuxSessions
	}
	inTmux, tmuxErr := sessions(ctx, tmux.Options{
		Socket:    tmux.SocketName(paths.WorkerDataDir()),
		TmpDir:    tmuxTmpDir,
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

func tmuxSessions(ctx context.Context, o tmux.Options) ([]string, error) {
	rt, err := tmux.New(o)
	if err != nil {
		return nil, err
	}
	return rt.List(ctx)
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
	args := make([]any, len(state.LivePhases))
	for i, phase := range state.LivePhases {
		args[i] = string(phase)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_runs WHERE phase IN (`+marks+`)`, args...).Scan(&n) //nolint:gosec // only placeholders are built into the query
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return n, nil
}
