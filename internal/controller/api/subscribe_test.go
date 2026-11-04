package api_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/e2e/netproxy"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// tape records the updates of a subscription.
type tape[S any] struct {
	mu      sync.Mutex
	updates []client.Update[S]
	at      []time.Time
}

func (p *tape[S]) handle(u client.Update[S]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, u)
	p.at = append(p.at, time.Now())
}

func (p *tape[S]) count(kind client.UpdateKind) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, u := range p.updates {
		if u.Kind == kind {
			n++
		}
	}
	return n
}

func (p *tape[S]) snapshot() []client.Update[S] {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.updates)
}

func (p *tape[S]) sequences() []uint64 {
	var out []uint64
	for _, u := range p.snapshot() {
		if u.Kind == client.UpdateEvent {
			out = append(out, u.Event.Sequence)
		}
	}
	return out
}

func subscribeIn[S any](t *testing.T, c *client.Client, view client.View[S], p *tape[S]) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Subscribe(ctx, c, view, p.handle) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && ctx.Err() == nil {
			t.Errorf("Subscribe: %v", err)
		}
	})
}

// waitFor polls cond, moving the client's fake clock a second each time round:
// the backoff and the refreshes only run when it moves.
func pollFor(t *testing.T, fake *clock.Fake, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
		if fake != nil {
			fake.Advance(time.Second)
		}
	}
	t.Fatalf("never happened: %s", what)
}

func newTasks(t *testing.T, tc *testController, n int, prefix string) {
	t.Helper()
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	makeTasks(t, tc, names...)
}

func lastSequence(t *testing.T, tc *testController) uint64 {
	t.Helper()
	page, err := tc.owner.Events(t.Context(), client.EventQuery{Tail: 1})
	must(t, err)
	n, err := strconv.ParseUint(page.Cursor, 10, 63)
	must(t, err)
	return n
}

func TestScenario_ClientSubscriptions_Reconexion(t *testing.T) {
	tc := newController(t, time.Now())
	newTasks(t, tc, 2, "before")

	proxy, err := netproxy.Listen(t.Context(), "127.0.0.1:0", func() string { return tc.addr })
	must(t, err)
	t.Cleanup(func() { _ = proxy.Close() })
	fake := clock.NewFake(time.Now())
	c, err := client.Open(tc.ownerDir, client.WithController(proxy.Addr()), client.WithClock(fake))
	must(t, err)

	p := &tape[v1.Overview]{}
	subscribeIn(t, c, client.OverviewView(), p)
	pollFor(t, nil, "the stream", func() bool { return p.count(client.UpdateConnected) == 1 })
	first := p.snapshot()[0]
	if first.Kind != client.UpdateSnapshot {
		t.Fatalf("the first update is %v", first.Kind)
	}
	from, err := strconv.ParseUint(first.State.Cursor, 10, 63)
	must(t, err)

	newTasks(t, tc, 3, "live")
	pollFor(t, nil, "the live events", func() bool { return len(p.sequences()) >= 3 })

	// The network is cut for ten seconds of the client's time, during which
	// the Controller keeps writing.
	proxy.SetMode(netproxy.Reject)
	pollFor(t, nil, "the banner", func() bool { return p.count(client.UpdateDisconnected) == 1 })
	newTasks(t, tc, 4, "outage")
	for range 10 {
		fake.Advance(time.Second)
		time.Sleep(10 * time.Millisecond)
	}
	if p.count(client.UpdateConnected) != 1 {
		t.Fatal("connected while the network was cut")
	}
	proxy.Heal()
	pollFor(t, fake, "the reconnection", func() bool { return p.count(client.UpdateConnected) == 2 })
	newTasks(t, tc, 2, "after")

	last := lastSequence(t, tc)
	pollFor(t, fake, "the last event", func() bool {
		seqs := p.sequences()
		return len(seqs) > 0 && seqs[len(seqs)-1] == last
	})

	var want []uint64
	for s := from + 1; s <= last; s++ {
		want = append(want, s)
	}
	if got := p.sequences(); !slices.Equal(got, want) {
		t.Fatalf("sequences = %v, want every one from %d to %d once", got, from+1, last)
	}
}

func TestScenario_ClientSubscriptions_CursorExpirado(t *testing.T) {
	tc := newController(t, time.Now())
	newTasks(t, tc, 3, "old")

	proxy, err := netproxy.Listen(t.Context(), "127.0.0.1:0", func() string { return tc.addr })
	must(t, err)
	t.Cleanup(func() { _ = proxy.Close() })
	fake := clock.NewFake(time.Now())
	c, err := client.Open(tc.ownerDir, client.WithController(proxy.Addr()), client.WithClock(fake))
	must(t, err)

	p := &tape[v1.Overview]{}
	subscribeIn(t, c, client.OverviewView(), p)
	pollFor(t, nil, "the stream", func() bool { return p.count(client.UpdateConnected) == 1 })

	// The retention purges everything held while the client is cut off.
	proxy.SetMode(netproxy.Reject)
	pollFor(t, nil, "the banner", func() bool { return p.count(client.UpdateDisconnected) == 1 })
	newTasks(t, tc, 2, "new")
	purged := lastSequence(t, tc)
	must(t, tc.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO controller_meta (key, value) VALUES ('events_purged_through', ?)`, strconv.FormatUint(purged, 10))
		return err
	}))
	newTasks(t, tc, 1, "fresh")
	proxy.Heal()

	pollFor(t, fake, "the resync", func() bool { return p.count(client.UpdateResync) == 1 })
	var resync client.Update[v1.Overview]
	for _, u := range p.snapshot() {
		if u.Kind == client.UpdateResync {
			resync = u
		}
	}
	last := lastSequence(t, tc)
	if resync.State.Cursor != strconv.FormatUint(last, 10) {
		t.Errorf("the resync is at %s, the history at %d", resync.State.Cursor, last)
	}
	if got := resync.State.Tasks["queued"] + resync.State.Tasks["blocked"] + resync.State.Tasks["waiting"]; got != 6 {
		t.Errorf("the resync counts %d Tasks of the 6 there are: %+v", got, resync.State.Tasks)
	}

	// And it goes on from there: the next event arrives once.
	newTasks(t, tc, 1, "next")
	next := lastSequence(t, tc)
	pollFor(t, fake, "the event after the resync", func() bool { return slices.Contains(p.sequences(), next) })
	seqs := p.sequences()
	if slices.Contains(seqs, last) || slices.Contains(seqs, purged) {
		t.Errorf("an event the resync already includes was delivered: %v", seqs)
	}
	if n := slices.Index(seqs, next); n != 0 || slices.Index(seqs[1:], next) >= 0 {
		t.Errorf("the event after the resync must arrive first and once: %v", seqs)
	}
}

func TestScenario_ClientSubscriptions_Carga(t *testing.T) {
	const (
		subscribers = 10
		emitted     = 30
	)
	tc := newController(t, time.Now())
	newTasks(t, tc, 1, "seed")

	type watcher struct {
		mu   sync.Mutex
		seen map[uint64]time.Time
	}
	watchers := make([]*watcher, subscribers)
	for i := range watchers {
		w := &watcher{seen: map[uint64]time.Time{}}
		watchers[i] = w
		c, _ := tc.enroll(fmt.Sprintf("viewer-%d", i))
		ready := make(chan struct{})
		var once sync.Once
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = client.Subscribe(ctx, c, client.OverviewView(), func(u client.Update[v1.Overview]) {
				switch u.Kind {
				case client.UpdateConnected:
					once.Do(func() { close(ready) })
				case client.UpdateEvent:
					w.mu.Lock()
					w.seen[u.Event.Sequence] = time.Now()
					w.mu.Unlock()
				}
			})
		}()
		t.Cleanup(func() { cancel(); <-done })
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatalf("subscriber %d never connected", i)
		}
	}

	from := lastSequence(t, tc)
	emittedAt := map[uint64]time.Time{}
	for i := range emitted {
		makeTasks(t, tc, fmt.Sprintf("load-%d", i))
		emittedAt[lastSequence(t, tc)] = time.Now()
	}

	var latencies []time.Duration
	for i, w := range watchers {
		pollFor(t, nil, fmt.Sprintf("subscriber %d to see every event", i), func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			for seq := range emittedAt {
				if _, ok := w.seen[seq]; !ok {
					return false
				}
			}
			return true
		})
		w.mu.Lock()
		for seq, at := range emittedAt {
			if seq > from {
				latencies = append(latencies, max(0, w.seen[seq].Sub(at)))
			}
		}
		w.mu.Unlock()
	}
	slices.Sort(latencies)
	if p95 := latencies[len(latencies)*95/100]; p95 > 2*time.Second {
		t.Fatalf("p95 latency %s over %d deliveries, want under 2s", p95, len(latencies))
	}
}
