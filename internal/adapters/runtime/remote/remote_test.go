package remote_test

import (
	"errors"
	"testing"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/remote"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/domain"
)

const run = "run_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"

func TestAttachArgv_ChoosesTheRuntimeByTheHandle(t *testing.T) {
	h := cfruntime.Handle{Runtime: tmux.Name, Ref: "cf-0123456789ab/" + domain.SessionName(run, 1)}
	argv, err := remote.AttachArgv(h, "/usr/bin/tmux", run, 1, false)
	if err != nil || argv[0] != "/usr/bin/tmux" || argv[5] != "attach-session" {
		t.Fatalf("argv = %q, %v", argv, err)
	}
	if _, err := remote.AttachArgv(cfruntime.Handle{Runtime: "pty", Ref: h.Ref}, "/usr/bin/tmux", run, 1, false); err == nil {
		t.Error("an unknown runtime was accepted")
	}
	if _, err := remote.AttachArgv(h, "/usr/bin/tmux", run, 2, false); !errors.Is(err, tmux.ErrNotAttachable) {
		t.Errorf("the session of another epoch: %v", err)
	}
}
