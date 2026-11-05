package tui

import (
	"context"
	"os/exec"

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
	Timeline(taskID string) (Timeline, error)
}

// Timeline is what the detail of a Task shows of its history.
type Timeline struct {
	Events []v1.Event
	// Capped says the history was too long to be read to its end: Events are
	// the newest of those read, and newer ones exist.
	Capped bool
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
	Tasks   FeedSource[v1.Task]
	Runs    FeedSource[v1.Run]
	Workers FeedSource[v1.Worker]
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
	timelinePage     = 100
	timelineKeep     = 50
	timelineMaxPages = 20 // pages read at most; beyond them the history is Capped
)

// timelineReader is the part of the Client SDK that the timeline needs.
type timelineReader interface {
	TaskTimeline(ctx context.Context, ref, project, after string, limit int) (v1.TaskTimeline, error)
}

// actionRunner is the part of actions.Runner that the backend uses.
type actionRunner interface {
	Run(ctx context.Context, id string, in actions.Input) actions.Result
	Attach(ctx context.Context, id string, in actions.Input) (*cli.AttachSession, *actions.Failure)
}

type sdkBackend struct {
	ctx    context.Context
	client timelineReader
	runner actionRunner
	// controller is the one the TUI was opened with (--controller, or the
	// stored one): every action goes to it, not to what the runner would dial.
	controller string
}

// NewBackend is the Backend over the Client SDK and the action registry. ctx
// bounds every call; the runner carries the composition of the binary (its
// Terminal).
func NewBackend(ctx context.Context, c *client.Client, runner actions.Runner) Backend {
	return sdkBackend{ctx: ctx, client: c, runner: runner, controller: c.Controller()}
}

// onController aims the input at the Controller of the TUI.
func (b sdkBackend) onController(in actions.Input) actions.Input {
	in.Controller = b.controller
	return in
}

func (b sdkBackend) Run(id string, in actions.Input) actions.Result {
	in = b.onController(in)
	return b.runner.Run(b.ctx, id, in)
}

func (b sdkBackend) Attach(id string, in actions.Input) (Attachment, *actions.Failure) {
	in = b.onController(in)
	session, failure := b.runner.Attach(b.ctx, id, in)
	if failure != nil {
		return nil, failure
	}
	return sdkAttachment{ctx: b.ctx, session: session}, nil
}

func (b sdkBackend) Timeline(taskID string) (Timeline, error) {
	var events []v1.Event
	after := ""
	for range timelineMaxPages {
		page, err := b.client.TaskTimeline(b.ctx, taskID, "", after, timelinePage)
		if err != nil {
			return Timeline{}, err
		}
		events = append(events, page.Events...)
		if len(events) > timelineKeep {
			events = events[len(events)-timelineKeep:]
		}
		if !page.HasMore || page.Cursor == "" {
			return Timeline{Events: events}, nil
		}
		after = page.Cursor
	}
	return Timeline{Events: events, Capped: true}, nil
}

type sdkAttachment struct {
	ctx     context.Context
	session *cli.AttachSession
}

func (a sdkAttachment) Cmd() *exec.Cmd { return a.session.Cmd }

func (a sdkAttachment) Finish(runErr error) error { return a.session.Finish(a.ctx, runErr) }
