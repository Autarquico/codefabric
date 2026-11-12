package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// The side of a Run in the socket its Worker lends it (see worker/runsocket):
// one JSON line of request and one of answer per connection. The Run holds no
// credentials of a client; the socket is its only way to the Controller.

// RunSocketEnv is the variable of the environment of a Run that holds the path of its socket.
const RunSocketEnv = "CF_RUN_SOCKET"

// Ops of a RunSocketRequest.
const (
	RunSocketOpPeers = "peers"
	RunSocketOpMove  = "move"
)

// RunSocketRequest is the one line a Run sends.
type RunSocketRequest struct {
	Op     string `json:"op"`
	Worker string `json:"worker,omitempty"`
}

// RunSocketError is why a request was refused: a CF_* code and a message made to be shown.
type RunSocketError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RunSocketError) Error() string { return e.Code + ": " + e.Message }

// RunSocketResponse is the one line the Worker answers with.
type RunSocketResponse struct {
	Error *RunSocketError      `json:"error,omitempty"`
	Peers []v1.RunPeer         `json:"peers,omitempty"`
	Move  *v1.MoveTaskResponse `json:"move,omitempty"`
}

const runSocketTimeout = 100 * time.Second

// CallRunSocket sends req to the socket at path and returns its answer. A
// refusal comes back as a *RunSocketError.
func CallRunSocket(ctx context.Context, path string, req RunSocketRequest) (RunSocketResponse, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return RunSocketResponse{}, fmt.Errorf("run socket: %w", err)
	}
	defer func() { _ = nc.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	} else {
		_ = nc.SetDeadline(time.Now().Add(runSocketTimeout))
	}
	if err := json.NewEncoder(nc).Encode(req); err != nil {
		return RunSocketResponse{}, fmt.Errorf("run socket: %w", err)
	}
	var resp RunSocketResponse
	if err := json.NewDecoder(io.LimitReader(nc, 1<<20)).Decode(&resp); err != nil {
		return RunSocketResponse{}, fmt.Errorf("run socket: reading the answer: %w", err)
	}
	if resp.Error != nil {
		return resp, resp.Error
	}
	return resp, nil
}
