package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

func TestPrepareAttemptsOnlyGrowOnlyWhileAcceptedAndSurviveReopening(t *testing.T) {
	db, path := open(t)
	k := state.Key{RunID: "run_a", LeaseEpoch: 1}
	next := func(db *state.DB) (uint64, error) {
		var n uint64
		err := db.WithTx(t.Context(), func(x *state.Tx) error {
			var err error
			n, err = state.NextPrepareAttempt(context.Background(), x, k)
			return err
		})
		return n, err
	}
	tx(t, db, func(x *state.Tx) error {
		_, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1))
		return err
	})
	if _, err := next(db); !errors.Is(err, state.ErrChanged) {
		t.Fatalf("an assignment not accepted took an attempt: %v", err)
	}
	tx(t, db, func(x *state.Tx) error {
		return state.Advance(t.Context(), x, k, state.PhaseAssigned, state.PhaseAccepted, t0)
	})
	for want := uint64(1); want <= 2; want++ {
		if got, err := next(db); err != nil || got != want {
			t.Fatalf("attempt = %d, %v; want %d", got, err, want)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if r, err := state.RunBy(t.Context(), db.Reader(), k); err != nil || r.PrepareAttempt != 2 {
		t.Fatalf("after reopening: %d, %v", r.PrepareAttempt, err)
	}
	if got, err := next(db); err != nil || got != 3 {
		t.Errorf("attempt = %d, %v; want 3", got, err)
	}
	tx(t, db, func(x *state.Tx) error {
		return state.Advance(t.Context(), x, k, state.PhaseAccepted, state.PhaseStarting, t0)
	})
	if _, err := next(db); !errors.Is(err, state.ErrChanged) {
		t.Errorf("a Run that is starting took an attempt: %v", err)
	}
}
