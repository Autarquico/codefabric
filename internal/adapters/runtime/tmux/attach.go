package tmux

import (
	"errors"
	"fmt"
	"path"
	"strings"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNotAttachable is returned for a handle or binary that this package would
// not have made, or that is not the session of the Run asked for.
var ErrNotAttachable = errors.New("tmux: not an attachable session")

// AttachArgv is the command that attaches to the session of a Run on the
// machine of its Worker. The handle comes from a database and the binary from
// a Worker, so nothing is taken as it is: the ref must be one this package
// writes, the session must be the one of that Run under that lease epoch (a
// Worker cannot hand over the session of another Run), and the binary an
// absolute, plain path. The tmux server is the Worker's (-L), and no
// configuration is read (-f /dev/null), as in every command the Worker runs.
// readOnly attaches with -r: what the person types does not reach the harness.
func AttachArgv(h cfruntime.Handle, binary, run string, epoch uint64, readOnly bool) ([]string, error) {
	if h.Runtime != Name {
		return nil, fmt.Errorf("%w: a handle of %q", ErrNotAttachable, h.Runtime)
	}
	socket, session, ok := parseRef(h.Ref)
	if !ok || strings.HasPrefix(socket, "-") {
		return nil, fmt.Errorf("%w: the handle's ref is malformed", ErrNotAttachable)
	}
	if gotRun, gotEpoch, ok := cfruntime.ParseName(session); !ok || gotRun != run || gotEpoch != epoch || session != domain.SessionName(run, epoch) {
		return nil, fmt.Errorf("%w: the session is not the one of that Run and lease epoch", ErrNotAttachable)
	}
	// The path comes from a Worker and is run on its machine as the user: it must
	// at least be called tmux, so that the program is not any program of the machine.
	if !domain.ValidAttachBinary(binary) || path.Base(binary) != "tmux" {
		return nil, fmt.Errorf("%w: the binary is not an absolute plain path to a program called tmux", ErrNotAttachable)
	}
	argv := []string{binary, "-L", socket, "-f", "/dev/null", "attach-session", "-t", "=" + session}
	if readOnly {
		argv = append(argv, "-r")
	}
	return argv, nil
}
