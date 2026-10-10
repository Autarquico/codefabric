package eventbus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/eventbus"
)

func TestNotifier_NotifyIsMonotonic(t *testing.T) {
	n := eventbus.NewNotifier()
	n.Notify(5)
	n.Notify(3)
	if got := n.Latest(); got != 5 {
		t.Fatalf("Latest = %d, want 5", got)
	}
}

func TestNotifier_WaitReturnsImmediatelyWhenAhead(t *testing.T) {
	n := eventbus.NewNotifier()
	n.Notify(10)
	got, err := n.Wait(t.Context(), 4)
	if err != nil || got != 10 {
		t.Fatalf("Wait = (%d, %v), want (10, nil)", got, err)
	}
}

func TestNotifier_WaitHonoursContext(t *testing.T) {
	n := eventbus.NewNotifier()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := n.Wait(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// Many waiters race many notifiers: every waiter must reach the final value,
// so none may miss a Notify issued between its check and its sleep.
func TestNotifier_WaitDoesNotMissConcurrentNotify(t *testing.T) {
	const waiters, target = 64, 500
	n := eventbus.NewNotifier()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for range waiters {
		wg.Go(func() {
			var seen uint64
			for seen < target {
				latest, err := n.Wait(ctx, seen)
				if err != nil {
					t.Errorf("Wait: %v", err)
					return
				}
				seen = latest
			}
		})
	}
	for seq := uint64(1); seq <= target; seq++ {
		wg.Go(func() { n.Notify(seq) })
	}
	wg.Wait()
	if got := n.Latest(); got != target {
		t.Fatalf("Latest = %d, want %d", got, target)
	}
}

func TestNotifier_ChangedIsClosedOnceTheMarkPassesAfter(t *testing.T) {
	n := eventbus.NewNotifier()
	n.Notify(5)

	select {
	case <-n.Changed(4):
	default:
		t.Fatal("Changed(4) is open with the mark at 5")
	}
	changed := n.Changed(5)
	select {
	case <-changed:
		t.Fatal("Changed(5) is closed with the mark at 5")
	default:
	}

	n.Notify(5) // not a rise
	select {
	case <-changed:
		t.Fatal("a Notify that does not raise the mark woke the waiter")
	default:
	}
	n.Notify(6)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("Changed(5) stayed open after the mark rose to 6")
	}
}
