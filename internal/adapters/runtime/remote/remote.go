// Package remote builds the command that attaches to a session from another
// machine, for whichever Runtime made the handle. It is apart from the port
// because the port cannot import the Runtimes that implement it.
package remote

import (
	"fmt"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
)

// AttachArgv is the command, as an argv for the machine of the session, that
// attaches to the session of the Run under the lease epoch. binary is the
// absolute path the Worker announced for the runtime of the handle.
func AttachArgv(h cfruntime.Handle, binary, run string, epoch uint64, readOnly bool) ([]string, error) {
	switch h.Runtime {
	case tmux.Name:
		return tmux.AttachArgv(h, binary, run, epoch, readOnly)
	}
	return nil, fmt.Errorf("remote: attaching to a %q session is not supported", h.Runtime)
}
