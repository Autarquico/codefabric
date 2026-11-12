// Package runsocket is how a Run asks its Worker to move its Task to another
// Worker (`/handto` inside a Run, `cf handoff --to w`), over a unix socket of the
// Run. The Run holds no credentials of any client: the socket is its only way
// to the Controller, and it only carries two requests.
//
//	{"op":"peers"}                  the Workers, by name and state
//	{"op":"move","worker":"name"}   move the Task of this Run to that Worker
//
// One request and one answer per connection, a JSON line each. The socket
// belongs to one (Run, lease epoch), fixed when the daemon makes it: the request
// carries no Run, so a Run cannot ask for another's Task. Whatever the op, the
// daemon (Handler) is who decides, and the Controller decides again by the lease.
// Any other op is refused and nothing else is reachable from here.
//
// The socket is 0600 in a 0700 directory: a process of the same user can still
// connect to it. The Runs of a Worker run as that user, so a Run that learns the
// path of another Run's socket could ask for that Run's Task to move: that is
// the limit of the design, and it can only ask for a move to a named Worker of
// the same fleet, audited as asked from that Run.
package runsocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// EnvVar is the variable of the environment of the harness that holds the path.
const EnvVar = client.RunSocketEnv

// MaxPath is the longest socket path accepted (sun_path is 104 bytes on macOS).
const MaxPath = 100

const (
	maxLine     = 4096
	maxConns    = 8
	ioTimeout   = 10 * time.Second
	callTimeout = 90 * time.Second
)

// The protocol, as the side of the Run (internal/client) speaks it.
type (
	// Request is the one line a Run sends.
	Request = client.RunSocketRequest
	// Response is the one line the Worker answers with.
	Response = client.RunSocketResponse
	// Error is why a request was refused: a CF_* code and a message made to be shown.
	Error = client.RunSocketError
)

// Ops of a Request.
const (
	OpPeers = client.RunSocketOpPeers
	OpMove  = client.RunSocketOpMove
)

// Call sends req to the socket at path and returns its answer; see client.CallRunSocket.
var Call = client.CallRunSocket

// Handler is what the daemon does for the Run a socket belongs to.
type Handler interface {
	Peers(ctx context.Context) ([]v1.RunPeer, error)
	Move(ctx context.Context, worker string) (v1.MoveTaskResponse, error)
}

// Dir is where the sockets of the Runs of a Worker are, under its data
// directory. It is short on purpose: the run directories are too deep for a
// unix socket.
func Dir(workerDataDir string) string { return filepath.Join(workerDataDir, "rs") }

// Path is the socket of one Run under one lease epoch.
func Path(dir, run string, leaseEpoch uint64) (string, error) {
	sum := sha256.Sum256([]byte(run + "/" + strconv.FormatUint(leaseEpoch, 10)))
	path := filepath.Join(dir, hex.EncodeToString(sum[:6])+".sock")
	if len(path) > MaxPath {
		return "", fmt.Errorf("runsocket: the socket path %q is %d bytes, over the %d a unix socket takes: the data directory of the Worker is too deep", path, len(path), MaxPath)
	}
	return path, nil
}

// Server serves the socket of one Run.
type Server struct {
	l    net.Listener
	path string
	h    Handler
	wg   sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc
}

// Listen makes the socket at path, 0600 in a 0700 directory, replacing the one a
// daemon that died left, and serves it until Close.
func Listen(path string, h Handler) (*Server, error) {
	if len(path) > MaxPath {
		return nil, fmt.Errorf("runsocket: the socket path is %d bytes, over the %d a unix socket takes", len(path), MaxPath)
	}
	if err := privateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("runsocket: removing a stale socket: %w", err)
	}
	l, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("runsocket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("runsocket: %w", err)
	}
	s := &Server{l: l, path: path, h: h}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Go(s.accept)
	return s, nil
}

// Path is where the Server listens.
func (s *Server) Path() string { return s.path }

// Close stops listening, ends what is in flight and removes the socket.
func (s *Server) Close() {
	s.cancel()
	_ = s.l.Close()
	s.wg.Wait()
	_ = os.Remove(s.path)
}

func (s *Server) accept() {
	sem := make(chan struct{}, maxConns)
	for {
		nc, err := s.l.Accept()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
			s.wg.Go(func() {
				defer func() { <-sem }()
				s.serve(nc)
			})
		default:
			_ = nc.Close()
		}
	}
}

func (s *Server) serve(nc net.Conn) {
	defer func() { _ = nc.Close() }()
	_ = nc.SetReadDeadline(time.Now().Add(ioTimeout))
	line, err := bufio.NewReaderSize(io.LimitReader(nc, maxLine+1), maxLine+1).ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return
	}
	var req Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var resp Response
	if len(line) > maxLine || dec.Decode(&req) != nil {
		resp.Error = &Error{Code: string(domain.CodeInvalidRequest), Message: "not a request this socket takes"}
	} else {
		resp = s.handle(req)
	}
	_ = nc.SetWriteDeadline(time.Now().Add(ioTimeout))
	_ = json.NewEncoder(nc).Encode(resp)
}

func (s *Server) handle(req Request) Response {
	ctx, cancel := context.WithTimeout(s.ctx, callTimeout)
	defer cancel()
	switch req.Op {
	case OpPeers:
		peers, err := s.h.Peers(ctx)
		if err != nil {
			return Response{Error: errorOf(err)}
		}
		return Response{Peers: peers}
	case OpMove:
		if req.Worker == "" {
			return Response{Error: &Error{Code: string(domain.CodeInvalidRequest), Message: "name the Worker to move to"}}
		}
		out, err := s.h.Move(ctx, req.Worker)
		if err != nil {
			return Response{Error: errorOf(err)}
		}
		return Response{Move: &out}
	}
	return Response{Error: &Error{Code: string(domain.CodeInvalidRequest), Message: "this socket takes only peers and move"}}
}

// errorOf is what a Run is told of an error: the code and message of one the
// Controller or the daemon coded, and nothing of any other.
func errorOf(err error) *Error {
	var de *domain.Error
	if errors.As(err, &de) {
		return &Error{Code: string(de.Code), Message: de.Message}
	}
	return &Error{Code: string(domain.CodeInternal), Message: "the Worker could not take the request to the Controller"}
}

// privateDir makes dir 0700, or refuses it: a directory that is not one, is not
// this user's, or that others can enter, would let them at the socket.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runsocket: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("runsocket: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		return fmt.Errorf("runsocket: the socket directory %s is not a directory", dir)
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("runsocket: the socket directory %s is not owned by this user", dir)
	case fi.Mode().Perm() != 0o700:
		return fmt.Errorf("runsocket: the socket directory %s is %v, not 0700", dir, fi.Mode().Perm())
	}
	return nil
}
