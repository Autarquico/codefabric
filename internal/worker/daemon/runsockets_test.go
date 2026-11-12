package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/runsocket"
)

type fakeRunController struct{ asked []string }

func (f *fakeRunController) RequestRunMove(_ context.Context, run string, req v1.RunMoveRequest) (v1.MoveTaskResponse, error) {
	f.asked = append(f.asked, run+"/"+req.Worker)
	if req.LeaseEpoch != 7 {
		panic("the epoch is the socket's, not the request's")
	}
	return v1.MoveTaskResponse{Move: v1.Move{ID: "op_1", Worker: req.Worker}}, nil
}

func (f *fakeRunController) RunPeers(context.Context) (v1.RunPeersResponse, error) {
	return v1.RunPeersResponse{Workers: []v1.RunPeer{{Name: "omarch01"}}}, nil
}

func TestScenario_RunSockets_EachRunAsksOnlyForItsOwnTask(t *testing.T) {
	dir, err := os.MkdirTemp("", "dm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctl := &fakeRunController{}
	socks := newRunSockets(dir, ctl, slog.New(slog.DiscardHandler))
	t.Cleanup(socks.CloseAll)

	one, err := socks.Serve("run_one", 7)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := socks.Serve("run_one", 7); err != nil || again != one {
		t.Fatalf("serving again = %q, %v; want the same socket %q", again, err, one)
	}
	other, err := socks.Serve("run_two", 7)
	if err != nil || other == one {
		t.Fatalf("second socket = %q, %v", other, err)
	}
	if info, err := os.Stat(one); err != nil || info.Mode().Perm() != 0o600 || filepath.Dir(one) != filepath.Join(dir, "rs") {
		t.Fatalf("socket %s: %v, %v", one, info, err)
	}

	if _, err := runsocket.Call(t.Context(), one, runsocket.Request{Op: runsocket.OpMove, Worker: "omarch02"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runsocket.Call(t.Context(), other, runsocket.Request{Op: runsocket.OpMove, Worker: "omarch01"}); err != nil {
		t.Fatal(err)
	}
	if len(ctl.asked) != 2 || ctl.asked[0] != "run_one/omarch02" || ctl.asked[1] != "run_two/omarch01" {
		t.Errorf("the Controller was asked %v: each socket must speak for its own Run", ctl.asked)
	}

	socks.Stop("run_one", 7)
	if _, err := os.Stat(one); !os.IsNotExist(err) {
		t.Errorf("the socket of a Run that ended is still there: %v", err)
	}
}
