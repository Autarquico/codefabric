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
	// History is the versions of a Workflow, newest first.
	History(workflow string) ([]v1.WorkflowVersion, error)
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
	Tasks     FeedSource[v1.Task]
	Runs      FeedSource[v1.Run]
	Workers   FeedSource[v1.Worker]
	Workflows FeedSource[WorkflowRow]
	Projects  FeedSource[v1.Project]
	// Output follows the output of one Run; the Logs view starts it when a Run
	// is picked and ends it when the person leaves that Run.
	Output OutputSource
}

// WorkflowRow is a Workflow with what the Workers hold of it: its drift and
// where it is applied are not part of the Workflow itself.
type WorkflowRow struct {
	Summary v1.WorkflowSummary
	Held    []v1.WorkflowState
}

// LogEvent is what a follow of output tells the Logs view. Exactly one of its
// parts is set: Data (bytes of the output; Gap says that bytes before them were
// never seen), Restart (a new connection or lease epoch begins: what was cut
// in the middle of a line will not continue), or Ended (why no more output is
// coming).
type LogEvent struct {
	Data    []byte
	Gap     bool
	Restart bool
	Ended   string
}

// OutputSource follows the output of a Run, delivering from the last bytes
// the Controller holds, until ctx ends or the output is over. Structured logs
// (levels, fields) will come as another source with the same shape, behind
// the same view.
type OutputSource func(ctx context.Context, run string, handle func(LogEvent)) error

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
		Workflows: func(ctx context.Context, h func(client.Update[[]WorkflowRow])) error {
			return client.Subscribe(ctx, c, workflowsView(), h)
		},
		Projects: func(ctx context.Context, h func(client.Update[[]v1.Project])) error {
			return client.Subscribe(ctx, c, projectsView(), h)
		},
		Output: func(ctx context.Context, run string, h func(LogEvent)) error {
			return followOutput(ctx, c, run, h)
		},
	}
}

// The Client SDK has ready-made views for the lists the Tasks, Runs and Workers
// views follow; these are the same kind for the other two.

func eventCursor(ctx context.Context, c *client.Client) (string, error) {
	page, err := c.Events(ctx, client.EventQuery{Tail: 1})
	return page.Cursor, err
}

// workflowsView is the Workflows with what the Workers hold of them. A Worker
// applying is not an event about the Workflow alone, so it is read as well
// every RefreshEvery.
func workflowsView() client.View[[]WorkflowRow] {
	return client.View[[]WorkflowRow]{
		Snapshot: func(ctx context.Context, c *client.Client) ([]WorkflowRow, string, error) {
			cursor, err := eventCursor(ctx, c)
			if err != nil {
				return nil, "", err
			}
			list, err := c.ListWorkflows(ctx)
			if err != nil {
				return nil, "", err
			}
			held, err := c.WorkflowState(ctx, "", "")
			if err != nil {
				return nil, "", err
			}
			rows := make([]WorkflowRow, len(list))
			for i, w := range list {
				rows[i].Summary = w
				for _, s := range held {
					if s.Workflow == w.Name {
						rows[i].Held = append(rows[i].Held, s)
					}
				}
			}
			return rows, cursor, nil
		},
		Invalidates: func(e v1.Event) bool { return e.WorkflowID != "" || e.WorkerID != "" },
		Refresh:     client.RefreshEvery,
	}
}

func projectsView() client.View[[]v1.Project] {
	return client.View[[]v1.Project]{
		Snapshot: func(ctx context.Context, c *client.Client) ([]v1.Project, string, error) {
			cursor, err := eventCursor(ctx, c)
			if err != nil {
				return nil, "", err
			}
			projects, err := c.Projects(ctx)
			return projects, cursor, err
		},
		Invalidates: func(e v1.Event) bool { return e.ProjectID != "" },
	}
}

// followOutput is the follow of `cf logs --follow`: from the tail the Controller
// holds, on to a new lease epoch when the Run goes on under one, until the
// output is closed or is not coming.
func followOutput(ctx context.Context, c *client.Client, run string, handle func(LogEvent)) error {
	query := client.OutputQuery{}
	for {
		ending, err := c.FollowOutput(ctx, run, query, client.OutputHandler{
			Begin: func(v1.OutputBegin) error { handle(LogEvent{Restart: true}); return nil },
			Chunk: func(ch v1.OutputChunk) error { handle(LogEvent{Data: ch.Data, Gap: ch.Gap}); return nil },
		})
		switch {
		case err != nil:
			return err
		case ending.End != nil && ending.End.Reason == v1.EndNewerEpoch:
			zero := uint64(0)
			query = client.OutputQuery{Epoch: ending.End.LeaseEpoch, Offset: &zero}
		case ending.End != nil:
			handle(LogEvent{Ended: "the Run is over and no more output is coming"})
			return nil
		default:
			handle(LogEvent{Ended: "the output is closed"})
			return nil
		}
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

// historyReader is the part of the Client SDK that a Workflow's history needs.
type historyReader interface {
	WorkflowHistory(ctx context.Context, name string) ([]v1.WorkflowVersion, error)
}

// actionRunner is the part of actions.Runner that the backend uses.
type actionRunner interface {
	Run(ctx context.Context, id string, in actions.Input) actions.Result
	Attach(ctx context.Context, id string, in actions.Input) (*cli.AttachSession, *actions.Failure)
}

type sdkBackend struct {
	ctx     context.Context
	client  timelineReader
	history historyReader
	runner  actionRunner
	// controller is the one the TUI was opened with (--controller, or the
	// stored one): every action goes to it, not to what the runner would dial.
	controller string
}

// NewBackend is the Backend over the Client SDK and the action registry. ctx
// bounds every call; the runner carries the composition of the binary (its
// Terminal).
func NewBackend(ctx context.Context, c *client.Client, runner actions.Runner) Backend {
	return sdkBackend{ctx: ctx, client: c, history: c, runner: runner, controller: c.Controller()}
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

func (b sdkBackend) History(workflow string) ([]v1.WorkflowVersion, error) {
	return b.history.WorkflowHistory(b.ctx, workflow)
}

type sdkAttachment struct {
	ctx     context.Context
	session *cli.AttachSession
}

func (a sdkAttachment) Cmd() *exec.Cmd { return a.session.Cmd }

func (a sdkAttachment) Finish(runErr error) error { return a.session.Finish(a.ctx, runErr) }
