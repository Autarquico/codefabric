package tui

import (
	"context"
	"os/exec"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/client"
)

// Backend is what the views ask of the Controller besides the feeds: the
// actions of the registry, the session of an attach, and a Task's timeline.
// The binary's is built over the Client SDK (NewBackend); tests give fakes.
type Backend interface {
	// Run executes a registry action in a fresh command tree.
	Run(id string, in actions.Input) actions.Result
	// Attach prepares an action that takes over the terminal.
	Attach(id string, in actions.Input) (Attachment, *actions.Failure)
	// Timeline is the latest events of a Task and its Runs, oldest first.
	Timeline(taskID string) ([]v1.Event, error)
}

// Attachment is a verified attach that the TUI runs with tea.ExecProcess.
type Attachment interface {
	Cmd() *exec.Cmd
	// Finish turns how the process ended into the answer `cf attach` gives.
	Finish(runErr error) error
}

// Feeds follow the lists of the views. A feed runs while its view is in the
// stack and ends when ctx does; like Source it returns when trying again would
// fail the same. A nil feed leaves its view with whatever the tests send it.
type Feeds struct {
	Tasks   func(ctx context.Context, handle func(client.Update[[]v1.Task])) error
	Runs    func(ctx context.Context, handle func(client.Update[[]v1.Run])) error
	Workers func(ctx context.Context, handle func(client.Update[[]v1.Worker])) error
}

// runner is how to follow the list behind route, or nil when there is none.
func (f Feeds) runner(route Route, send func(tea.Msg)) func(ctx context.Context) error {
	switch {
	case route == RouteTasks && f.Tasks != nil:
		return func(ctx context.Context) error { return f.Tasks(ctx, func(u tasksMsg) { send(u) }) }
	case route == RouteRuns && f.Runs != nil:
		return func(ctx context.Context) error { return f.Runs(ctx, func(u runsMsg) { send(u) }) }
	case route == RouteWorkers && f.Workers != nil:
		return func(ctx context.Context) error { return f.Workers(ctx, func(u workersMsg) { send(u) }) }
	}
	return nil
}

// SubscribeFeeds are the Feeds that follow the Controller.
func SubscribeFeeds(c *client.Client) Feeds {
	return Feeds{
		Tasks: func(ctx context.Context, h func(client.Update[[]v1.Task])) error {
			return client.Subscribe(ctx, c, client.TasksView(client.TaskQuery{}), h)
		},
		Runs: func(ctx context.Context, h func(client.Update[[]v1.Run])) error {
			return client.Subscribe(ctx, c, client.RunsView(client.RunQuery{}), h)
		},
		Workers: func(ctx context.Context, h func(client.Update[[]v1.Worker])) error {
			return client.Subscribe(ctx, c, client.WorkersView(false), h)
		},
	}
}

// Timeline limits: a Task's history is read from the start in pages and only
// the newest events are kept, which are the ones the detail pane has room for.
const (
	timelinePage = 100
	timelineKeep = 50
	timelinePags = 20 // pages read at most; beyond them the newest of those read is shown
)

type sdkBackend struct {
	ctx    context.Context
	client *client.Client
	runner actions.Runner
}

// NewBackend is the Backend over the Client SDK and the action registry. ctx
// bounds every call; the runner carries the composition of the binary (its
// Terminal).
func NewBackend(ctx context.Context, c *client.Client, runner actions.Runner) Backend {
	return sdkBackend{ctx: ctx, client: c, runner: runner}
}

func (b sdkBackend) Run(id string, in actions.Input) actions.Result {
	return b.runner.Run(b.ctx, id, in)
}

func (b sdkBackend) Attach(id string, in actions.Input) (Attachment, *actions.Failure) {
	session, failure := b.runner.Attach(b.ctx, id, in)
	if failure != nil {
		return nil, failure
	}
	return sdkAttachment{ctx: b.ctx, session: session}, nil
}

func (b sdkBackend) Timeline(taskID string) ([]v1.Event, error) {
	var events []v1.Event
	after := ""
	for range timelinePags {
		page, err := b.client.TaskTimeline(b.ctx, taskID, "", after, timelinePage)
		if err != nil {
			return nil, err
		}
		events = append(events, page.Events...)
		if len(events) > timelineKeep {
			events = events[len(events)-timelineKeep:]
		}
		if !page.HasMore || page.Cursor == "" {
			break
		}
		after = page.Cursor
	}
	return events, nil
}

type sdkAttachment struct {
	ctx     context.Context
	session *cli.AttachSession
}

func (a sdkAttachment) Cmd() *exec.Cmd { return a.session.Cmd }

func (a sdkAttachment) Finish(runErr error) error { return a.session.Finish(a.ctx, runErr) }
