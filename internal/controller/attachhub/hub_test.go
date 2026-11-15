package attachhub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func wantCode(t *testing.T, err error, code domain.Code) {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

// live makes worker count as polling, and returns a function that ends the poll
// and gives what it took.
func live(t *testing.T, h *Hub, _ *clock.Fake, worker domain.ID) (stop func() []Grant) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan []Grant, 1)
	go func() { reqs, _ := h.Poll(ctx, worker, time.Hour); done <- reqs }()
	deadline := time.Now().Add(5 * time.Second)
	for !h.Live(worker) {
		if time.Now().After(deadline) {
			t.Fatal("the poll never started")
		}
		time.Sleep(time.Millisecond)
	}
	return func() []Grant { cancel(); return <-done }
}

func newHub(cfg Config) (*Hub, *clock.Fake) {
	clk := clock.NewFake(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	cfg.Clock = clk
	return New(cfg), clk
}

func TestOffer_NeedsALivePoll(t *testing.T) {
	h, clk := newHub(Config{})
	g := Grant{Worker: "worker_a", Run: "run_1", Mode: "view"}
	_, err := h.Offer(g)
	wantCode(t, err, domain.CodeWorkerOffline)

	stop := live(t, h, clk, "worker_a")
	stop()
	// Within the grace after a poll the Worker is asking again at once.
	if !h.Live("worker_a") {
		t.Error("a Worker is not live right after its poll ended")
	}
	clk.Advance(DefaultPollGrace + time.Second)
	if h.Live("worker_a") {
		t.Error("a Worker that stopped polling is still live after the grace")
	}
}

func TestOffer_QueueAndSessionsAreBounded(t *testing.T) {
	h, clk := newHub(Config{QueuePerWorker: 2, MaxSessions: 3})
	// A poll that is not waiting on the queue: the Worker is live, nothing is taken.
	h.mu.Lock()
	h.worker("worker_a").polls = 1
	h.worker("worker_b").polls = 1
	h.mu.Unlock()
	g := Grant{Worker: "worker_a", Run: "run_1", Mode: "view"}
	for range 2 {
		if _, err := h.Offer(g); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.Offer(g)
	wantCode(t, err, domain.CodeWorkerBusy)
	if _, err := h.Offer(Grant{Worker: "worker_b", Run: "run_2", Mode: "view"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.Offer(Grant{Worker: "worker_b", Run: "run_2", Mode: "view"})
	wantCode(t, err, domain.CodeInProgress)

	// What expired gives its place back.
	clk.Advance(DefaultPendingTTL + time.Second)
	if _, err := h.Offer(g); err != nil {
		t.Errorf("after the expiry: %v", err)
	}
}

func TestPoll_ReturnsAfterTheWaitWithNothing(t *testing.T) {
	h, clk := newHub(Config{})
	got := make(chan []Grant, 1)
	go func() { reqs, _ := h.Poll(t.Context(), "worker_a", 20*time.Second); got <- reqs }()
	if err := clk.BlockUntil(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(21 * time.Second)
	select {
	case reqs := <-got:
		if len(reqs) != 0 {
			t.Errorf("requests = %+v", reqs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll did not end")
	}
}

func TestClaim_OnlyADeliveredGrantOfItsWorkerOnce(t *testing.T) {
	validations := 0
	h, clk := newHub(Config{Validate: func(context.Context, Grant) error { validations++; return nil }})
	stop := live(t, h, clk, "worker_a")
	p, err := h.Offer(Grant{Worker: "worker_a", Run: "run_1", Epoch: 1, Mode: "view"})
	if err != nil {
		t.Fatal(err)
	}
	id := p.Grant().ID
	if reqs := stop(); len(reqs) != 1 || reqs[0].ID != id {
		t.Fatalf("poll = %+v", reqs)
	}
	_, err = h.Claim(t.Context(), "worker_a", "no-such")
	wantCode(t, err, domain.CodeNotFound)
	_, err = h.Claim(t.Context(), "worker_b", id)
	wantCode(t, err, domain.CodePolicyDenied)
	if validations != 0 {
		t.Error("the grant was validated for a Worker that is not its own")
	}
	if _, err := h.Claim(t.Context(), "worker_a", id); err != nil {
		t.Fatal(err)
	}
	_, err = h.Claim(t.Context(), "worker_a", id)
	wantCode(t, err, domain.CodeNotFound)
	if validations != 1 {
		t.Errorf("validations = %d, want 1", validations)
	}
}

func TestClaim_AGrantThatIsNotDeliveredCannotBeClaimed(t *testing.T) {
	h, clk := newHub(Config{})
	// A Worker that is live but whose poll takes nothing, so the grant stays queued.
	_ = clk
	h.mu.Lock()
	h.worker("worker_b").polls = 1
	h.mu.Unlock()
	p, err := h.Offer(Grant{Worker: "worker_b", Run: "run_1", Mode: "view"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Claim(t.Context(), "worker_b", p.Grant().ID)
	wantCode(t, err, domain.CodeNotFound)
}

func TestClaim_AStaleEpochKillsTheGrant(t *testing.T) {
	h, clk := newHub(Config{Validate: func(context.Context, Grant) error {
		return domain.NewError(domain.CodeStaleLease, "moved", nil)
	}})
	stop := live(t, h, clk, "worker_a")
	p, err := h.Offer(Grant{Worker: "worker_a", Run: "run_1", Epoch: 1, Mode: "view"})
	if err != nil {
		t.Fatal(err)
	}
	reqs := stop()
	if len(reqs) != 1 {
		t.Fatalf("poll = %+v", reqs)
	}
	_, err = h.Claim(t.Context(), "worker_a", reqs[0].ID)
	wantCode(t, err, domain.CodeStaleLease)
	wantCode(t, p.Wait(t.Context()), domain.CodeStaleLease)
	if h.Open() != 0 {
		t.Errorf("open = %d", h.Open())
	}
}

func TestShutdown_EndsWhatWaits(t *testing.T) {
	h, clk := newHub(Config{})
	stop := live(t, h, clk, "worker_a")
	defer stop()
	h.mu.Lock()
	h.worker("worker_b").polls = 1
	h.mu.Unlock()
	p, err := h.Offer(Grant{Worker: "worker_b", Run: "run_1", Mode: "view"})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- p.Wait(t.Context()) }()
	h.Shutdown()
	select {
	case err := <-waited:
		wantCode(t, err, domain.CodeControllerUnreachable)
	case <-time.After(5 * time.Second):
		t.Fatal("a request kept waiting after the shutdown")
	}
	_, err = h.Offer(Grant{Worker: "worker_a", Run: "run_1", Mode: "view"})
	if !IsClosed(err) {
		t.Errorf("offer after shutdown: %v", err)
	}
}
