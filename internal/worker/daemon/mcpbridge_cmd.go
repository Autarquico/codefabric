package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/inputsock"
)

// approveTool is the name of the MCP tool of the bridge: Claude Code is launched with
// --permission-prompt-tool mcp__cf__approve, and calls it for every permission
// it would ask a person.
const approveTool = "approve"

// mcpProtocolVersion is answered to a client that names none.
const mcpProtocolVersion = "2025-06-18"

// confirmWait is how long the bridge, once its harness has closed its stdin,
// waits for the daemon to record the answers it has returned and the questions
// it withdrew. Before that, it tells the daemon for as long as the bridge lives:
// the harness waits for the end of its input, which waits for those records.
const confirmWait = 2 * time.Second

// newMCPBridgeCmd is the MCP server Claude Code is launched with to ask
// permissions (started by the harness, not by hand): a minimal stdio server
// with one tool, approve, which asks the daemon of the Worker over the socket
// of the Run and returns what a person answered.
func newMCPBridgeCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:           "mcp-bridge --socket <path>",
		Short:         "Serve the permission tool of a Run over MCP (started by the harness, not by hand)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if socket == "" {
				return errors.New("mcp-bridge: no --socket")
			}
			ask := newSocketAsker(&inputsock.Client{Path: socket})
			return serveMCP(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), ask)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "the unix socket of the daemon for this Run")
	return cmd
}

// asker answers one permission: the tool and its input, the decision.
type asker interface {
	ask(ctx context.Context, tool string, input json.RawMessage) (decision, message string, err error)
	// wait gives the daemon up to d to hear that the answers reached the harness.
	wait(d time.Duration)
}

type socketAsker struct {
	client *inputsock.Client
	// confirms (acks and withdrawals) outlive the call that made them, and the
	// context of the server.
	confirms sync.WaitGroup
}

func newSocketAsker(c *inputsock.Client) *socketAsker { return &socketAsker{client: c} }

func (a *socketAsker) ask(ctx context.Context, tool string, input json.RawMessage) (string, string, error) {
	id, err := newPromptID()
	if err != nil {
		return "", "", err
	}
	ans, err := a.client.Ask(ctx, inputsock.Message{PromptID: id, Tool: tool, Input: input})
	life := context.WithoutCancel(ctx)
	if err != nil {
		// The call was cancelled: its prompt is not to wait in the inbox, nor to
		// hold the input of the harness open.
		a.confirms.Go(func() { _ = a.client.Withdraw(life, id) })
		return "", "", err
	}
	// The harness is about to be told; the daemon is told after, and as many
	// times as it takes. It is not the harness's to wait for.
	a.confirms.Go(func() { _ = a.client.Confirm(life, id) })
	return ans.Decision, ans.Message, nil
}

func (a *socketAsker) wait(d time.Duration) {
	done := make(chan struct{})
	go func() { a.confirms.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// newPromptID is the ID of a question, made here so that asking again after
// the daemon restarts is asking the same question.
func newPromptID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp-bridge: %w", err)
	}
	return "p" + hex.EncodeToString(b[:]), nil
}

// rpcRequest is a JSON-RPC 2.0 message of a client. Without an ID it is a
// notification, which is never answered.
type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveMCP is the MCP stdio transport: one JSON message per line. It returns
// when the client closes its side or ctx ends. Calls of the tool wait for a
// person, so each runs apart from the loop that reads the next message.
func serveMCP(ctx context.Context, in io.Reader, out io.Writer, a asker) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		writeMu sync.Mutex
		calls   sync.WaitGroup
		mu      sync.Mutex
		pending = map[string]context.CancelFunc{}
	)
	reply := func(r rpcResponse) {
		r.JSONRPC = "2.0"
		raw, err := json.Marshal(r)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = out.Write(append(raw, '\n'))
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, inputsock.MaxLine)
	for sc.Scan() {
		var req rpcRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.Method == "" {
			continue
		}
		notification := len(req.ID) == 0
		switch req.Method {
		case "initialize":
			reply(rpcResponse{ID: req.ID, Result: initializeResult(req.Params)})
		case "ping":
			reply(rpcResponse{ID: req.ID, Result: struct{}{}})
		case "tools/list":
			reply(rpcResponse{ID: req.ID, Result: map[string]any{"tools": []any{approveToolSpec()}}})
		case "tools/call":
			callCtx, callCancel := context.WithCancel(ctx)
			mu.Lock()
			pending[string(req.ID)] = callCancel
			mu.Unlock()
			calls.Go(func() {
				defer callCancel()
				res, ok := approve(callCtx, a, req.Params)
				mu.Lock()
				delete(pending, string(req.ID))
				mu.Unlock()
				if ok {
					reply(rpcResponse{ID: req.ID, Result: res})
				}
			})
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				mu.Lock()
				if c := pending[string(p.RequestID)]; c != nil {
					c()
				}
				mu.Unlock()
			}
		default:
			if !notification {
				reply(rpcResponse{ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found"}})
			}
		}
	}
	// The client is gone: what it was waiting for is not answered any more.
	cancel()
	calls.Wait()
	a.wait(confirmWait)
	return sc.Err()
}

func initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ProtocolVersion == "" {
		p.ProtocolVersion = mcpProtocolVersion
	}
	return map[string]any{
		"protocolVersion": p.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "cf", "version": "1"},
	}
}

func approveToolSpec() map[string]any {
	return map[string]any{
		"name":        approveTool,
		"description": "Asks a person, through CodeFabric, whether a tool may run",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name":   map[string]any{"type": "string"},
				"input":       map[string]any{"type": "object"},
				"tool_use_id": map[string]any{"type": "string"},
			},
			"required": []string{"tool_name", "input"},
		},
	}
}

// approve is one call of the tool. ok is false when the call was cancelled:
// a cancelled request is not answered.
func approve(ctx context.Context, a asker, params json.RawMessage) (result any, ok bool) {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil || p.Name != approveTool {
		return toolResult(`{"behavior":"deny","message":"unknown tool"}`, true), true
	}
	input := p.Arguments.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	decision, message, err := a.ask(ctx, p.Arguments.ToolName, input)
	if err != nil {
		return nil, false
	}
	if decision == domain.AnswerAllow {
		// The input goes back as it came: an allow that changes nothing.
		return toolResult(mustJSON(map[string]any{"behavior": "allow", "updatedInput": input}), false), true
	}
	if message == "" {
		message = "denied"
	}
	return toolResult(mustJSON(map[string]any{"behavior": "deny", "message": message}), false), true
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `{"behavior":"deny","message":"internal error"}`
	}
	return string(raw)
}
