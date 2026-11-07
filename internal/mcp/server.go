// Package mcp serves CodeFabric to a model over the Model Context Protocol.
// It is a client of the Controller like the CLI and the TUI (ADR 0001): every
// tool is one call of the Client SDK, and the profile decides which tools
// exist at all.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Profile is what a model may do (§16.3). Each includes the one before it.
type Profile string

// The profiles of `cf mcp serve`.
const (
	Read    Profile = "read"
	Execute Profile = "execute"
	Modify  Profile = "modify"
)

// ParseProfile reads the value of --profile.
func ParseProfile(s string) (Profile, error) {
	switch p := Profile(s); p {
	case Read, Execute, Modify:
		return p, nil
	}
	return "", domain.NewError(domain.CodeInvalidRequest,
		fmt.Sprintf("unknown profile %q: use read, execute or modify", s), nil)
}

func (p Profile) rank() int {
	switch p {
	case Read:
		return 0
	case Execute:
		return 1
	default:
		return 2
	}
}

// Client is what the tools use of *client.Client.
type Client interface {
	Projects(ctx context.Context) ([]v1.Project, error)
	Project(ctx context.Context, ref string) (v1.Project, error)
	Tasks(ctx context.Context, q client.TaskQuery) ([]v1.Task, error)
	Task(ctx context.Context, ref, project string) (v1.Task, error)
	Runs(ctx context.Context, q client.RunQuery) ([]v1.Run, error)
	Run(ctx context.Context, ref string) (v1.Run, error)
	RunOutput(ctx context.Context, ref string, q client.OutputQuery) (v1.RunOutput, error)
	Workers(ctx context.Context, includeRemoved bool) ([]v1.Worker, error)
	Worker(ctx context.Context, ref string) (v1.Worker, error)
	ListWorkflows(ctx context.Context) ([]v1.WorkflowSummary, error)
	ShowWorkflow(ctx context.Context, ref string) (v1.WorkflowVersion, error)
	ApplyWorkflow(ctx context.Context, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error)
	Inbox(ctx context.Context) (v1.InboxResponse, error)
	Search(ctx context.Context, q client.SearchQuery) (v1.SearchResponse, error)
	Logs(ctx context.Context, q client.LogQuery) (v1.LogPage, error)
	Events(ctx context.Context, q client.EventQuery) (v1.EventPage, error)
	FollowEvents(ctx context.Context, q client.EventQuery, fn func(v1.Event) error) error
	NewTask(ctx context.Context, dir string, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error)
	RespondTask(ctx context.Context, ref, project string, req v1.RespondRequest) (v1.RespondResponse, error)
	CancelTask(ctx context.Context, ref, project, requestID string) (v1.Task, error)
	DrainWorker(ctx context.Context, ref, requestID string) (v1.Worker, error)
	UndrainWorker(ctx context.Context, ref, requestID string) (v1.Worker, error)
}

var _ Client = (*client.Client)(nil)

const (
	defaultWait = 5 * time.Minute
	maxWait     = 10 * time.Minute
)

// NewServer builds the MCP server of a profile. Only the tools the profile
// allows are registered, so asking for another is an unknown tool. Wire names
// have no dots (the Anthropic API admits ^[a-zA-Z0-9_-]{1,128}$); the
// permissions and the docs keep the names of §17.2.
func NewServer(profile Profile, c Client, clk clock.Clock) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "codefabric", Version: buildinfo.Get().Version}, nil)
	t := tools{c: c, clk: clk}
	t.read(s)
	if profile.rank() >= Execute.rank() {
		t.execute(s)
	}
	if profile.rank() >= Modify.rank() {
		t.modify(s)
	}
	return s
}

// add registers a tool whose handler returns its result as any value. The
// output schema is declared by hand and kept small: the SDK would derive one
// from the Go type and fail on the first field it cannot describe. A
// *domain.Error comes back as a tool error that keeps its code and details;
// the SDK handles the unexpected ones.
func add[In any](s *sdk.Server, name, description string, out *jsonschema.Schema, h func(context.Context, In) (any, error)) {
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: description, OutputSchema: out},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
			res, err := h(ctx, in)
			var coded *domain.Error
			if errors.As(err, &coded) {
				return toolError(coded), nil, nil
			}
			return nil, res, err
		})
}

func toolError(e *domain.Error) *sdk.CallToolResult {
	structured := map[string]any{"code": e.Code, "message": e.Message}
	if e.Details != nil {
		structured["details"] = e.Details
	}
	return &sdk.CallToolResult{
		IsError:           true,
		Content:           []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("%s: %s", e.Code, e.Message)}},
		StructuredContent: structured,
	}
}

// objectOf is the schema of an object with the named properties, all required.
// Types are the JSON ones; what is inside an array or an entity is documented
// by the description of the tool, not nested here.
func objectOf(props map[string]string) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
	for name, typ := range props {
		s.Properties[name] = &jsonschema.Schema{Type: typ}
		s.Required = append(s.Required, name)
	}
	return s
}

// entity is the schema of one Controller entity as the API shows it.
var entity = &jsonschema.Schema{Type: "object"}

// Serve runs s over in and out until in ends or ctx is done. out carries only
// JSON-RPC: whatever else there is to say goes to the diagnostics of the caller.
func Serve(ctx context.Context, s *sdk.Server, in io.Reader, out io.Writer) error {
	return s.Run(ctx, &sdk.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func requireRequestID(id string) error {
	if strings.TrimSpace(id) == "" {
		return domain.NewError(domain.CodeInvalidRequest, "request_id is required for a tool that changes something", nil)
	}
	return nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
