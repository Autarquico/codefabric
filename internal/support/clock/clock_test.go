package clock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

func TestFake_Advance(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)

	f.Advance(90 * time.Second)
	if got, want := f.Now(), start.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("after Advance: got %v, want %v", got, want)
	}
}

func TestFake_ConcurrentUse(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				f.Advance(time.Millisecond)
				_ = f.Now()
			}
		})
	}
	wg.Wait()
	if got, want := f.Now(), time.Unix(0, 0).Add(800*time.Millisecond); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReal_ReturnsUTC(t *testing.T) {
	if loc := (clock.Real{}).Now().Location(); loc != time.UTC {
		t.Fatalf("location = %v, want UTC", loc)
	}
}

func fired[T any](c <-chan T) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func TestFake_TimerFiresOnceWhenAdvancedPastIt(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	timer := f.NewTimer(time.Minute)

	f.Advance(time.Minute - time.Nanosecond)
	if fired(timer.C()) {
		t.Fatal("fired early")
	}
	f.Advance(time.Nanosecond)
	if !fired(timer.C()) {
		t.Fatal("did not fire at its time")
	}
	f.Advance(time.Hour)
	if fired(timer.C()) {
		t.Fatal("fired twice")
	}
}

func TestFake_TickerFiresOncePerAdvanceHoweverManyPeriodsPass(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	ticker := f.NewTicker(time.Second)

	f.Advance(10 * time.Second)
	if !fired(ticker.C()) || fired(ticker.C()) {
		t.Fatal("want exactly one tick for ten periods")
	}
	f.Advance(time.Second - time.Nanosecond)
	if fired(ticker.C()) {
		t.Fatal("ticked before the next period")
	}
	f.Advance(time.Nanosecond)
	if !fired(ticker.C()) {
		t.Fatal("did not tick on the next period")
	}
}

func TestFake_StoppedTimersDoNotFire(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	timer, ticker := f.NewTimer(time.Second), f.NewTicker(time.Second)
	timer.Stop()
	ticker.Stop()

	f.Advance(time.Hour)
	if fired(timer.C()) || fired(ticker.C()) {
		t.Fatal("a stopped timer fired")
	}
}

func TestFake_BlockUntilWaitsForTheTimers(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	released := make(chan struct{})
	go func() {
		if err := f.BlockUntil(t.Context(), 2); err != nil {
			t.Error(err)
		}
		close(released)
	}()

	f.NewTimer(time.Second)
	if fired(released) {
		t.Fatal("released with one timer of two")
	}
	f.NewTicker(time.Second)
	<-released
}

func TestReal_TimersFire(*testing.T) {
	var r clock.Real
	timer, ticker := r.NewTimer(time.Millisecond), r.NewTicker(time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	<-timer.C()
	<-ticker.C()
}

func TestFake_BlockUntilEndsWithItsContext(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.BlockUntil(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestFake_ATickerNeedsAPositiveInterval(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewTicker(%s) did not panic", d)
				}
			}()
			clock.NewFake(time.Unix(0, 0)).NewTicker(d)
		}()
	}
}

func TestFake_ALongAdvanceKeepsTheTickerPhase(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	ticker := f.NewTicker(time.Nanosecond * 3)
	f.Advance(1000 * time.Hour) // 1.2e15 periods: must not loop over them
	if !fired(ticker.C()) {
		t.Fatal("did not tick")
	}
	ticker2 := f.NewTicker(time.Second)
	f.Advance(10*time.Second + 500*time.Millisecond)
	if !fired(ticker2.C()) {
		t.Fatal("did not tick")
	}
	f.Advance(499 * time.Millisecond)
	if fired(ticker2.C()) {
		t.Fatal("ticked off its phase")
	}
	f.Advance(time.Millisecond)
	if !fired(ticker2.C()) {
		t.Fatal("did not tick on its phase")
	}
}

func TestFake_StopDropsATickAlreadySent(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	timer, ticker := f.NewTimer(time.Second), f.NewTicker(time.Second)
	f.Advance(time.Second)
	timer.Stop()
	ticker.Stop()
	if fired(timer.C()) || fired(ticker.C()) {
		t.Fatal("a stopped timer delivered a tick")
	}
}

func TestFake_StopRacesWithAdvance(*testing.T) {
	f := clock.NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for range 50 {
		ticker := f.NewTicker(time.Millisecond)
		wg.Add(2)
		go func() { defer wg.Done(); f.Advance(time.Millisecond) }()
		go func() { defer wg.Done(); ticker.Stop() }()
	}
	wg.Wait()
}
