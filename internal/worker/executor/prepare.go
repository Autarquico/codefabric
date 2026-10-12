package executor

import (
	"context"
	"fmt"
	"os"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Preparer makes the directory a Run executes in. git-relay-publication
// replaces the default with a worktree of the source commit; until then it is
// an empty directory.
type Preparer func(ctx context.Context, run state.Run, dir string) error

func emptyWorktree(_ context.Context, run state.Run, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making the worktree of %s: %w", run.RunID, err)
	}
	return nil
}
