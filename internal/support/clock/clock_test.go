package clock_test

import (
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
