package tmux

import (
	"context"
	"fmt"
	"strings"

	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
	"github.com/Autarquico/codeflock/internal/domain"
)

// snapshotArgs is the tmux command that prints the screen of a session: -p to
// stdout, -e to keep its colours and attributes. The target is the active pane of
// the session, matched exactly (the "=" prefix) and not by prefix.
func snapshotArgs(session string) []string {
	return []string{"capture-pane", "-p", "-e", "-t", "=" + session + ":"}
}

// Snapshot is the screen of the session of h as it is now, SGR attributes
// included. It reads the pane and takes no client: it changes nothing in the
// session and does not resize it. A session that is not there is a
// CF_RUN_NOT_ATTACHABLE.
func (r *Runtime) Snapshot(ctx context.Context, h cfruntime.Handle) ([]byte, error) {
	session, err := r.session(h)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := r.exec(ctx, snapshotArgs(session)...)
	switch {
	case err == nil:
		return []byte(stdout), nil
	case isMissing(err):
		return nil, domain.NewError(domain.CodeRunNotAttachable, "the session of this Run is not on this Worker",
			map[string]any{"reason": domain.ReasonNotRunning})
	}
	return nil, fmt.Errorf("tmux: capturing session %s: %w (%s)", session, err, strings.TrimSpace(stderr))
}
