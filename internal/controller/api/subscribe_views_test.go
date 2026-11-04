package api_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// lastState is the state of the last snapshot the tape holds.
func lastState[S any](p *tape[S]) (S, bool) {
	var state S
	found := false
	for _, u := range p.snapshot() {
		if u.Kind == client.UpdateSnapshot {
			state, found = u.State, true
		}
	}
	return state, found
}

func TestSubscribe_TasksViewReadsTheListAgainWhenATaskChanges(t *testing.T) {
	tc := newController(t, time.Now())
	makeTasks(t, tc, "one")
	p := &tape[[]v1.Task]{}
	subscribeIn(t, tc.owner, client.TasksView(client.TaskQuery{}), p)
	pollFor(t, nil, "the first snapshot", func() bool { return p.count(client.UpdateSnapshot) == 1 })

	makeTasks(t, tc, "two")
	pollFor(t, nil, "the list with the new Task", func() bool {
		state, _ := lastState(p)
		return len(state) == 2
	})
}

func TestSubscribe_RunsViewIgnoresWhatIsNotARunAndReadsAgainOnARun(t *testing.T) {
	tc := newController(t, time.Now())
	p := &tape[[]v1.Run]{}
	makeTasks(t, tc, "seed") // creates the project too
	subscribeIn(t, tc.owner, client.RunsView(client.RunQuery{}), p)
	pollFor(t, nil, "the first snapshot", func() bool { return p.count(client.UpdateSnapshot) == 1 })

	makeTasks(t, tc, "no-run") // an event with no Run in it
	before := lastSequence(t, tc)
	pollFor(t, nil, "that event", func() bool { return len(p.sequences()) > 0 && p.sequences()[len(p.sequences())-1] == before })

	if _, err := tc.owner.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Name: "with-run", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA}); err != nil {
		t.Fatal(err)
	}
	pollFor(t, nil, "the list with the new Run", func() bool {
		state, _ := lastState(p)
		return len(state) == 1
	})
	if n := p.count(client.UpdateSnapshot); n != 2 {
		t.Errorf("%d snapshots; the Task with no Run must not have caused one", n)
	}
}

func TestSubscribe_WorkersViewReadsAgainOnAWorkerAndOnTheClock(t *testing.T) {
	tc := newController(t, time.Now())
	fake := clock.NewFake(time.Now())
	c, err := client.Open(tc.ownerDir, client.WithClock(fake))
	must(t, err)
	p := &tape[[]v1.Worker]{}
	subscribeIn(t, c, client.WorkersView(false), p)
	pollFor(t, nil, "the stream", func() bool { return p.count(client.UpdateConnected) == 1 })

	tc.enrollRealWorker("w1")
	pollFor(t, fake, "the list with the Worker", func() bool {
		state, _ := lastState(p)
		return len(state) == 1
	})

	// Its health is not an event: the clock alone makes the view read again.
	seen := p.count(client.UpdateSnapshot)
	pollFor(t, fake, "a refresh by the clock", func() bool { return p.count(client.UpdateSnapshot) > seen })
}
