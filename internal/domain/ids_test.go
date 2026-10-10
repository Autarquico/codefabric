package domain

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestIDGenerator_IDsInTheSameMillisecondIncrease(t *testing.T) {
	fixed := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	gen := NewIDGenerator(func() time.Time { return fixed }, rand.Reader)
	previous := gen.New(KindRun)
	for range 100 {
		next := gen.New(KindRun)
		if next <= previous {
			t.Fatalf("ids must increase: %s then %s", previous, next)
		}
		previous = next
	}
}

func TestIDGenerator_ConcurrentCallsYieldUniqueIDs(t *testing.T) {
	const goroutines, perGoroutine = 16, 200
	gen := NewIDGenerator(time.Now, rand.Reader)
	ids := make(chan ID, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				ids <- gen.New(KindRun)
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[ID]bool, goroutines*perGoroutine)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestParseID_RoundTripsGeneratedIDs(t *testing.T) {
	gen := NewIDGenerator(time.Now, rand.Reader)
	for kind := range knownKinds {
		id := gen.New(kind)
		got, _, err := ParseID(string(id))
		if err != nil || got != kind {
			t.Errorf("ParseID(%s) = %q, %v; want %q", id, got, err, kind)
		}
	}
}

func TestParseID_RejectsMalformedText(t *testing.T) {
	for _, text := range []string{"", "run", "run_", "run_short", "nope_01J8ZQ4M5N6P7R8S9T0V1W2X3Y", "01J8ZQ4M5N6P7R8S9T0V1W2X3Y"} {
		if _, _, err := ParseID(text); !errors.Is(err, ErrMalformedID) {
			t.Errorf("ParseID(%q) error = %v, want ErrMalformedID", text, err)
		}
	}
}
