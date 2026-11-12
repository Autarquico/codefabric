package runsocket_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/runsocket"
)

type fakeHandler struct {
	moved []string
	err   error
}

func (f *fakeHandler) Peers(context.Context) ([]v1.RunPeer, error) {
	return []v1.RunPeer{{Name: "omarch01", State: domain.WorkerOnline}}, f.err
}

func (f *fakeHandler) Move(_ context.Context, worker string) (v1.MoveTaskResponse, error) {
	f.moved = append(f.moved, worker)
	return v1.MoveTaskResponse{Move: v1.Move{ID: "op_1", Worker: worker, Stage: "checkpointing"}}, f.err
}

func listen(t *testing.T, h runsocket.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path, err := runsocket.Path(filepath.Join(dir, "rs"), "run_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := runsocket.Listen(path, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return path
}

func TestScenario_SocketIsPrivate(t *testing.T) {
	path := listen(t, &fakeHandler{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode = %v, want a socket 0600", info.Mode())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", dir.Mode().Perm())
	}
}

func TestScenario_ListenRefusesADirectoryOthersCanEnter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil { //nolint:gosec // a directory others can enter, on purpose
		t.Fatal(err)
	}
	if _, err := runsocket.Listen(filepath.Join(dir, "a.sock"), &fakeHandler{}); err == nil {
		t.Fatal("a socket was made in a directory others can enter")
	}
}

func TestScenario_OnlyPeersAndMoveAreTaken(t *testing.T) {
	h := &fakeHandler{}
	path := listen(t, h)
	ctx := t.Context()

	resp, err := runsocket.Call(ctx, path, runsocket.Request{Op: runsocket.OpPeers})
	if err != nil || len(resp.Peers) != 1 || resp.Peers[0].Name != "omarch01" {
		t.Fatalf("peers = %+v, %v", resp, err)
	}
	resp, err = runsocket.Call(ctx, path, runsocket.Request{Op: runsocket.OpMove, Worker: "omarch02"})
	if err != nil || resp.Move == nil || resp.Move.Move.Worker != "omarch02" {
		t.Fatalf("move = %+v, %v", resp, err)
	}
	for _, req := range []runsocket.Request{{Op: "cancel"}, {Op: "secrets"}, {Op: ""}, {Op: runsocket.OpMove}} {
		_, err := runsocket.Call(ctx, path, req)
		var refused *runsocket.Error
		if !errors.As(err, &refused) || refused.Code != string(domain.CodeInvalidRequest) {
			t.Errorf("op %q: err = %v, want CF_INVALID_REQUEST", req.Op, err)
		}
	}
	if len(h.moved) != 1 {
		t.Errorf("the handler moved %v, want only omarch02", h.moved)
	}
}

func TestScenario_UnknownFieldsAndGarbageAreRefused(t *testing.T) {
	path := listen(t, &fakeHandler{})
	for _, raw := range []string{`{"op":"move","worker":"a","run":"run_2"}` + "\n", "not json\n", strings.Repeat("x", 8000) + "\n"} {
		nc, err := new(net.Dialer).DialContext(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := nc.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 512)
		n, _ := nc.Read(buf)
		_ = nc.Close()
		if !strings.Contains(string(buf[:n]), "CF_INVALID_REQUEST") {
			t.Errorf("%.20q: answer %q, want CF_INVALID_REQUEST", raw, buf[:n])
		}
	}
}

func TestScenario_ErrorsToTheRunCarryNoInternals(t *testing.T) {
	path := listen(t, &fakeHandler{err: errors.New("sql: /var/lib/cf/state.db locked")})
	_, err := runsocket.Call(t.Context(), path, runsocket.Request{Op: runsocket.OpMove, Worker: "a"})
	var refused *runsocket.Error
	if !errors.As(err, &refused) || refused.Code != string(domain.CodeInternal) || strings.Contains(refused.Message, "sql") {
		t.Fatalf("err = %v", err)
	}

	path = listen(t, &fakeHandler{err: domain.NewError(domain.CodeStaleLease, "this is not the live lease of the Run", nil)})
	_, err = runsocket.Call(t.Context(), path, runsocket.Request{Op: runsocket.OpMove, Worker: "a"})
	if !errors.As(err, &refused) || refused.Code != string(domain.CodeStaleLease) {
		t.Fatalf("err = %v, want CF_STALE_LEASE", err)
	}
}
