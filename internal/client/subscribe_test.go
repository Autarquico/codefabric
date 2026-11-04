package client

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// recorder collects what a subscription tells its handler.
type recorder struct {
	mu      sync.Mutex
	updates []Update[int]
}

func (r *recorder) handle(u Update[int]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, u)
}

func (r *recorder) all() []Update[int] {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.updates)
}

func (r *recorder) kinds() []UpdateKind {
	var out []UpdateKind
	for _, u := range r.all() {
		out = append(out, u.Kind)
	}
	return out
}

func (r *recorder) sequences() []uint64 {
	var out []uint64
	for _, u := range r.all() {
		if u.Kind == UpdateEvent {
			out = append(out, u.Event.Sequence)
		}
	}
	return out
}

// eventually polls cond for a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("never happened: %s", what)
}

// run starts a subscription and stops it, and checks its error, with the test.
func run(t *testing.T, c *Client, view View[int], r *recorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Subscribe(ctx, c, view, r.handle) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Subscribe ended with %v", err)
		}
	})
}

func counting(cursor string, calls *atomic.Int32) View[int] {
	return View[int]{Snapshot: func(context.Context, *Client) (int, string, error) {
		return int(calls.Add(1)), cursor, nil
	}}
}

func streamOf(frames ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			sseWrite(w, f)
		}
		<-r.Context().Done()
	})
}

func TestSubscribe_AppliesEachSequenceOnceAndInOrder(t *testing.T) {
	// 3 and 4 are at or below the snapshot's cursor, 6 and 5 come twice.
	c := testClient(t, streamOf(eventFrame(3), eventFrame(4), eventFrame(5), eventFrame(6), eventFrame(5), eventFrame(6), eventFrame(7)))
	var calls atomic.Int32
	view := counting("4", &calls)
	view.Invalidates = func(v1.Event) bool { return false }
	r := &recorder{}
	run(t, c, view, r)

	eventually(t, "event 7", func() bool { return slices.Contains(r.sequences(), 7) })
	if got := r.sequences(); !slices.Equal(got, []uint64{5, 6, 7}) {
		t.Fatalf("sequences = %v", got)
	}
	if calls.Load() != 1 {
		t.Errorf("the snapshot was read %d times; no event invalidates it", calls.Load())
	}
}

func TestSubscribe_AnInvalidatingEventReadsTheSnapshotAgain(t *testing.T) {
	c := testClient(t, streamOf(eventFrame(1), eventFrame(2)))
	var calls atomic.Int32
	view := counting("0", &calls)
	view.Invalidates = func(e v1.Event) bool { return e.Sequence == 2 }
	r := &recorder{}
	run(t, c, view, r)

	eventually(t, "the second snapshot", func() bool {
		for _, u := range r.all() {
			if u.Kind == UpdateSnapshot && u.State == 2 {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 2 {
		t.Errorf("snapshot read %d times, want 2: event 1 does not invalidate it", calls.Load())
	}
}

func TestSubscribe_NoInvalidatesMeansEveryEventDoes(t *testing.T) {
	c := testClient(t, streamOf(eventFrame(1)))
	var calls atomic.Int32
	r := &recorder{}
	run(t, c, counting("0", &calls), r)
	eventually(t, "a refetch", func() bool { return calls.Load() >= 2 })
}

func TestSubscribe_RetriesTheFirstSnapshotWhileTheControllerIsDown(t *testing.T) {
	c := testClient(t, streamOf(eventFrame(1)))
	var calls atomic.Int32
	view := View[int]{
		Snapshot: func(context.Context, *Client) (int, string, error) {
			if calls.Add(1) <= 2 {
				return 0, "", domain.NewError(domain.CodeControllerUnreachable, "down", nil)
			}
			return 7, "0", nil
		},
		Invalidates: func(v1.Event) bool { return false },
	}
	r := &recorder{}
	run(t, c, view, r)

	eventually(t, "the event", func() bool { return len(r.sequences()) == 1 })
	want := []UpdateKind{UpdateDisconnected, UpdateSnapshot, UpdateConnected, UpdateEvent}
	if got := r.kinds(); !slices.Equal(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	if u := r.all()[0]; !isCode(u.Err, domain.CodeControllerUnreachable) {
		t.Errorf("disconnected for %v", u.Err)
	}
	if calls.Load() != 3 {
		t.Errorf("snapshot tried %d times, want 3", calls.Load())
	}
}

func TestSubscribe_AFailureThatWouldRepeatEndsIt(t *testing.T) {
	c := testClient(t, streamOf())
	view := View[int]{Snapshot: func(context.Context, *Client) (int, string, error) {
		return 0, "", domain.NewError(domain.CodeUnauthenticated, "no", nil)
	}}
	err := Subscribe(t.Context(), c, view, func(Update[int]) {})
	if !isCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubscribe_ACursorTheControllerLostResyncs(t *testing.T) {
	var opens atomic.Int32
	var lastIDs sync.Map
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := opens.Add(1)
		lastIDs.Store(n, r.Header.Get("Last-Event-ID"))
		if n == 1 {
			status(http.StatusConflict, domain.CodeCursorExpired)(w)
			return
		}
		streamOf(eventFrame(21)).ServeHTTP(w, r)
	}))
	var calls atomic.Int32
	view := View[int]{
		Snapshot: func(context.Context, *Client) (int, string, error) {
			n := int(calls.Add(1))
			return n, map[int]string{1: "3", 2: "20"}[n], nil
		},
		Invalidates: func(v1.Event) bool { return false },
	}
	r := &recorder{}
	run(t, c, view, r)

	eventually(t, "the event after the resync", func() bool { return len(r.sequences()) == 1 })
	want := []UpdateKind{UpdateSnapshot, UpdateResync, UpdateConnected, UpdateEvent}
	if got := r.kinds(); !slices.Equal(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	if u := r.all()[1]; u.State != 2 {
		t.Errorf("the resync carries snapshot %d, want the second", u.State)
	}
	if got, _ := lastIDs.Load(int32(2)); got != "20" {
		t.Errorf("resumed from %q, want the cursor of the new snapshot", got)
	}
}

func TestSubscribe_RefreshReadsTheSnapshotEveryPeriodOnTheClock(t *testing.T) {
	c := testClient(t, streamOf())
	fake := clock.NewFake(time.Unix(0, 0))
	c.clock = fake
	c.timing.idle = time.Hour
	var calls atomic.Int32
	view := counting("0", &calls)
	view.Invalidates = func(v1.Event) bool { return false }
	view.Refresh = 5 * time.Second
	r := &recorder{}
	run(t, c, view, r)

	for want := int32(2); want <= 3; want++ {
		if err := fake.BlockUntil(t.Context(), 2); err != nil { // the idle timer of the stream, and the refresh
			t.Fatal(err)
		}
		fake.Advance(4 * time.Second)
		if got := calls.Load(); got != want-1 {
			t.Fatalf("read %d times before the period was over, want %d", got, want-1)
		}
		fake.Advance(time.Second)
		eventually(t, "a refresh", func() bool { return calls.Load() == want })
	}
}

// A refetch must not move the stream: the cursor of a snapshot is usually ahead
// of the events that have reached the client, and those still have to be told.
func TestSubscribe_ARefetchAheadOfTheStreamSkipsNoEvent(t *testing.T) {
	refetched := make(chan struct{})
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseWrite(w, eventFrame(1))
		select {
		case <-refetched:
		case <-r.Context().Done():
			return
		}
		for seq := uint64(2); seq <= 5; seq++ {
			sseWrite(w, eventFrame(seq))
		}
		<-r.Context().Done()
	}))
	var calls atomic.Int32
	view := View[int]{
		Snapshot: func(context.Context, *Client) (int, string, error) {
			n := calls.Add(1)
			if n == 2 {
				close(refetched)
				return 2, "5", nil // already includes 2..5, which have not streamed yet
			}
			return int(n), "0", nil
		},
		Invalidates: func(e v1.Event) bool { return e.Sequence == 1 },
	}
	r := &recorder{}
	run(t, c, view, r)

	eventually(t, "event 5", func() bool { return slices.Contains(r.sequences(), 5) })
	if got := r.sequences(); !slices.Equal(got, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("sequences = %v, want every one once", got)
	}
}

func TestSubscribe_ABurstOfEventsReadsTheSnapshotAtMostOncePerMinimumInterval(t *testing.T) {
	second := make(chan struct{})
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for seq := uint64(1); seq <= 3; seq++ {
			sseWrite(w, eventFrame(seq))
		}
		select {
		case <-second:
		case <-r.Context().Done():
			return
		}
		for seq := uint64(4); seq <= 13; seq++ {
			sseWrite(w, eventFrame(seq))
		}
		<-r.Context().Done()
	}))
	fake := clock.NewFake(time.Unix(0, 0))
	c.clock = fake
	c.timing.idle = time.Hour // its timer is the one that is always there
	c.timing.minRefetch = time.Second
	var calls atomic.Int32
	r := &recorder{}
	run(t, c, counting("0", &calls), r)

	eventually(t, "the refetch of the first burst", func() bool { return calls.Load() == 2 })
	close(second)
	eventually(t, "the second burst", func() bool { return slices.Contains(r.sequences(), 13) })
	if err := fake.BlockUntil(t.Context(), 2); err != nil { // the idle timer of the stream, and the wait
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("the snapshot was read %d times inside the minimum interval, want 2", got)
	}
	fake.Advance(time.Second)
	eventually(t, "one read for the whole second burst", func() bool { return calls.Load() == 3 })

	// Nothing is left pending: no timer waits for another read.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := fake.BlockUntil(ctx, 2); err == nil {
		t.Errorf("another read is waiting after %d", calls.Load())
	}
}

func TestEventCursorIsTheLatestSequenceOfTheController(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" || r.URL.Query().Get("tail") != "1" {
			t.Errorf("asked for %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"events":[],"cursor":"42","has_more":false}`))
	}))
	got, err := c.eventCursor(t.Context())
	if err != nil || got != "42" {
		t.Fatalf("eventCursor = %q, %v", got, err)
	}
}

func TestListViewsTakeTheCursorBeforeTheyReadTheList(t *testing.T) {
	var order []string
	var mu sync.Mutex
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/events":
			_, _ = w.Write([]byte(`{"events":[],"cursor":"7","has_more":false}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	for name, snapshot := range map[string]func() (string, error){
		"tasks": func() (string, error) {
			_, cur, err := TasksView(TaskQuery{}).Snapshot(t.Context(), c)
			return cur, err
		},
		"runs":    func() (string, error) { _, cur, err := RunsView(RunQuery{}).Snapshot(t.Context(), c); return cur, err },
		"workers": func() (string, error) { _, cur, err := WorkersView(false).Snapshot(t.Context(), c); return cur, err },
	} {
		mu.Lock()
		order = nil
		mu.Unlock()
		cur, err := snapshot()
		if err != nil || cur != "7" {
			t.Errorf("%s: cursor %q, %v", name, cur, err)
		}
		mu.Lock()
		if len(order) != 2 || order[0] != "/v1/events" {
			t.Errorf("%s: requests %v, want the cursor first", name, order)
		}
		mu.Unlock()
	}
}

func TestReadyViewsInvalidateOnTheirOwnEvents(t *testing.T) {
	cases := []struct {
		name        string
		invalidates func(v1.Event) bool
		task, run   bool
		worker      bool
	}{
		{"tasks", TasksView(TaskQuery{}).Invalidates, true, true, false},
		{"runs", RunsView(RunQuery{}).Invalidates, false, true, false},
		{"workers", WorkersView(false).Invalidates, false, false, true},
	}
	for _, tc := range cases {
		if got := tc.invalidates(v1.Event{TaskID: "t"}); got != tc.task {
			t.Errorf("%s on a Task event = %v", tc.name, got)
		}
		if got := tc.invalidates(v1.Event{RunID: "r"}); got != tc.run {
			t.Errorf("%s on a Run event = %v", tc.name, got)
		}
		if got := tc.invalidates(v1.Event{WorkerID: "w"}); got != tc.worker {
			t.Errorf("%s on a Worker event = %v", tc.name, got)
		}
		if tc.invalidates(v1.Event{}) {
			t.Errorf("%s on an event of nothing it shows", tc.name)
		}
	}
	if WorkersView(false).Refresh != RefreshEvery || OverviewView().Refresh != RefreshEvery || TasksView(TaskQuery{}).Refresh != 0 {
		t.Error("only the views of health refresh by the clock")
	}
}
