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

func TestSubscribe_RefreshReadsTheSnapshotOnTheClock(t *testing.T) {
	c := testClient(t, streamOf())
	fake := clock.NewFake(time.Unix(0, 0))
	c.clock = fake
	var calls atomic.Int32
	view := counting("0", &calls)
	view.Invalidates = func(v1.Event) bool { return false }
	view.Refresh = 5 * time.Second
	r := &recorder{}
	run(t, c, view, r)

	for want := int32(2); want <= 3; want++ {
		eventually(t, "a refresh", func() bool {
			fake.Advance(5 * time.Second)
			return calls.Load() >= want
		})
	}
}
