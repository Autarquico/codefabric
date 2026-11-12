package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// fakeRunSocket answers the requests of a Run as the Worker would: a move to
// "nowhere" is refused, any other is taken. It records the requests it saw.
func fakeRunSocket(t *testing.T) (path string, requests func() []client.RunSocketRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ho")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = dir + "/s.sock"
	l, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var (
		mu   sync.Mutex
		seen []client.RunSocketRequest
	)
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			var req client.RunSocketRequest
			_ = json.NewDecoder(nc).Decode(&req)
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			var resp client.RunSocketResponse
			switch {
			case req.Op == client.RunSocketOpPeers:
				resp.Peers = []v1.RunPeer{{Name: "omarch01", State: domain.WorkerOnline}}
			case req.Worker == "nowhere":
				resp.Error = &client.RunSocketError{Code: string(domain.CodeNotFound), Message: `no Worker "nowhere"`}
			default:
				resp.Move = &v1.MoveTaskResponse{Task: v1.Task{Name: "fix-login"}, Move: v1.Move{ID: "op_1", Worker: req.Worker, Stage: "checkpointing"}}
			}
			_ = json.NewEncoder(nc).Encode(resp)
			_ = nc.Close()
		}
	}()
	return path, func() []client.RunSocketRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func runHandoff(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunContext(t.Context(), append([]string{"handoff"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestScenario_HandoffInsideARunUsesTheSocketOfTheRun(t *testing.T) {
	path, requests := fakeRunSocket(t)
	t.Setenv(client.RunSocketEnv, path)
	t.Setenv("HOME", t.TempDir()) // no credentials of a client anywhere

	code, out, errOut := runHandoff(t, "--session", "s-1", "--to", "omarch02")
	got := requests()
	if code != 0 || !strings.Contains(out, "fix-login") || !strings.Contains(out, "cf attach fix-login") ||
		len(got) != 1 || got[0] != (client.RunSocketRequest{Op: "move", Worker: "omarch02"}) {
		t.Fatalf("exit %d, out %q, err %q, requests %v", code, out, errOut, got)
	}

	code, out, _ = runHandoff(t, "--list")
	if code != 0 || !strings.Contains(out, "omarch01") {
		t.Errorf("--list: exit %d, out %q", code, out)
	}

	code, out, errOut = runHandoff(t, "--to", "nowhere")
	if code == 0 || !strings.Contains(out+errOut, "CF_NOT_FOUND") {
		t.Errorf("a refusal: exit %d, out %q, err %q; want the CF_NOT_FOUND code shown", code, out, errOut)
	}
	if code, _, errOut = runHandoff(t); code == 0 || !strings.Contains(errOut, "CF_INVALID_REQUEST") {
		t.Errorf("no --to: exit %d, err %q", code, errOut)
	}
}

func TestScenario_HandoffOutsideARunNeedsItsClientSide(t *testing.T) {
	t.Setenv(client.RunSocketEnv, "")
	if code, _, errOut := runHandoff(t, "--to", "omarch02"); code == 0 || !strings.Contains(errOut, "CF_INVALID_REQUEST") {
		t.Errorf("exit %d, err %q", code, errOut)
	}
}
