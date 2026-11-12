package daemon

import (
	"context"
	"log/slog"
	"sync"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/runsocket"
)

// runController is what the sockets of the Runs need of the Controller, with the
// credentials of this Worker. Nothing else of the Worker's reach is lent to a Run.
type runController interface {
	RequestRunMove(ctx context.Context, run string, req v1.RunMoveRequest) (v1.MoveTaskResponse, error)
	RunPeers(ctx context.Context) (v1.RunPeersResponse, error)
}

// runSockets serves the socket of each interactive Run (executor.RunSockets).
// What a Run can do through it is what runHandler does: list the Workers by name
// and ask for the move of its own Task.
type runSockets struct {
	dir string
	c   runController
	log *slog.Logger

	mu      sync.Mutex
	servers map[runKey]*runsocket.Server
}

type runKey struct {
	run   string
	epoch uint64
}

func newRunSockets(workerDataDir string, c runController, log *slog.Logger) *runSockets {
	return &runSockets{dir: runsocket.Dir(workerDataDir), c: c, log: log, servers: map[runKey]*runsocket.Server{}}
}

// Serve makes the socket of the Run under the lease epoch if it has none.
func (r *runSockets) Serve(run string, epoch uint64) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := runKey{run, epoch}
	if s := r.servers[k]; s != nil {
		return s.Path(), nil
	}
	path, err := runsocket.Path(r.dir, run, epoch)
	if err != nil {
		return "", err
	}
	s, err := runsocket.Listen(path, runHandler{c: r.c, run: run, epoch: epoch, log: r.log})
	if err != nil {
		return "", err
	}
	r.servers[k] = s
	return path, nil
}

// Stop takes the socket of the Run down.
func (r *runSockets) Stop(run string, epoch uint64) {
	r.mu.Lock()
	s := r.servers[runKey{run, epoch}]
	delete(r.servers, runKey{run, epoch})
	r.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// CloseAll takes every socket down, when the daemon ends.
func (r *runSockets) CloseAll() {
	r.mu.Lock()
	servers := r.servers
	r.servers = map[runKey]*runsocket.Server{}
	r.mu.Unlock()
	for _, s := range servers {
		s.Close()
	}
}

// runHandler answers for one Run: the Run and the epoch are fixed here, not
// taken from the request, so the Run can only ask for its own Task.
type runHandler struct {
	c     runController
	run   string
	epoch uint64
	log   *slog.Logger
}

func (h runHandler) Peers(ctx context.Context) ([]v1.RunPeer, error) {
	out, err := h.c.RunPeers(ctx)
	return out.Workers, err
}

func (h runHandler) Move(ctx context.Context, worker string) (v1.MoveTaskResponse, error) {
	out, err := h.c.RequestRunMove(ctx, h.run, v1.RunMoveRequest{LeaseEpoch: h.epoch, Worker: worker})
	if err != nil {
		h.log.WarnContext(ctx, "a Run asked to move its Task and was refused", "run", h.run, "worker", worker, "err", err)
		return out, err
	}
	h.log.InfoContext(ctx, "a Run asked to move its Task", "run", h.run, "worker", worker, "move", out.Move.ID)
	return out, nil
}
