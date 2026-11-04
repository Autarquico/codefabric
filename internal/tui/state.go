package tui

import (
	"context"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Source feeds the shell with the updates of the overview. The binary's is the
// subscription of the Client SDK; tests give it fixtures. It returns when ctx
// ends or when trying again would fail the same.
type Source func(ctx context.Context, handle func(OverviewUpdate)) error

// SubscribeOverview is the Source that follows the Controller.
func SubscribeOverview(c *client.Client) Source {
	return func(ctx context.Context, handle func(OverviewUpdate)) error {
		return client.Subscribe(ctx, c, client.OverviewView(), handle)
	}
}

// Connection is what the shell knows about the Controller.
type Connection int

// States of the connection.
const (
	Connecting Connection = iota
	Connected
	Disconnected
)

// State is what the screens draw, fed by the updates of the Source.
type State struct {
	Connection Connection
	Loaded     bool
	Overview   v1.Overview
	Activity   activityLog

	// The lists of the views, while they are followed.
	Tasks     Feed[v1.Task]
	Runs      Feed[v1.Run]
	Workers   Feed[v1.Worker]
	Workflows Feed[WorkflowRow]
	Projects  Feed[v1.Project]
}

// Feed is the list that a view follows: the items of the last snapshot, whether
// there was one, and whether the Controller is out of reach.
type Feed[T any] struct {
	Items  []T
	Loaded bool
	Down   bool
	Err    error // why the feed gave up
}

func (f *Feed[T]) apply(u client.Update[[]T]) {
	switch u.Kind {
	case client.UpdateSnapshot, client.UpdateResync:
		f.Items, f.Loaded = u.State, true
	case client.UpdateConnected:
		f.Down = false
	case client.UpdateDisconnected:
		f.Down = true
	}
}

// apply folds an update into the state.
func (s *State) apply(u OverviewUpdate) {
	switch u.Kind {
	case client.UpdateSnapshot:
		s.Overview, s.Loaded = u.State, true
		s.Activity.merge(u.State.Activity...)
	case client.UpdateResync:
		s.Overview, s.Loaded = u.State, true
		s.Activity.reset(u.State.Activity...)
	case client.UpdateEvent:
		s.Activity.merge(u.Event)
	case client.UpdateConnected:
		s.Connection = Connected
	case client.UpdateDisconnected:
		s.Connection = Disconnected
	}
}

// running is the Tasks being worked on.
func (s *State) running() int { return s.Overview.Tasks[domain.TaskRunning] }

// workersOnline and workersTotal count the Workers that are not removed.
func (s *State) workersOnline() int { return s.Overview.Workers[domain.WorkerOnline] }

func (s *State) workersTotal() int {
	n := 0
	for _, c := range s.Overview.Workers {
		n += c
	}
	return n
}

// degraded says whether a Worker that should be serving is not.
func (s *State) degraded() bool {
	w := s.Overview.Workers
	return w[domain.WorkerDegraded]+w[domain.WorkerOffline] > 0
}

// OverviewUpdate is what the Source tells the shell.
type OverviewUpdate = client.Update[v1.Overview]
