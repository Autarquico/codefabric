package state_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Closing the capture of one checkpoint leaves the other the Run has open as it is.
func TestCapture_EndingOneLeavesTheOthersOfTheRunOpen(t *testing.T) {
	db, _ := open(t)
	k := state.Key{RunID: "run_cap", LeaseEpoch: 1}
	tx(t, db, func(x *state.Tx) error {
		for _, id := range []string{"cp_old", "cp_new"} {
			c := state.Capture{Key: k, CheckpointID: id, Trigger: "interval", Ref: "r/" + id, Live: true, CreatedAt: t0}
			if _, err := state.BeginCapture(t.Context(), x, c, state.PhaseRunning); err != nil {
				return err
			}
		}
		ended, err := state.EndCapture(t.Context(), x, k, "cp_old", state.CaptureFailed, t0)
		if !ended || err != nil {
			t.Fatalf("ended = %v, %v", ended, err)
		}
		return nil
	})
	var stillOpen []string
	for _, id := range []string{"cp_old", "cp_new"} {
		var done bool
		err := db.Reader().QueryRowContext(t.Context(),
			`SELECT done_at IS NOT NULL FROM checkpoint_captures WHERE run_id = ? AND checkpoint_id = ?`, k.RunID, id).Scan(&done)
		if err != nil {
			t.Fatal(err)
		}
		if !done {
			stillOpen = append(stillOpen, id)
		}
	}
	if len(stillOpen) != 1 || stillOpen[0] != "cp_new" {
		t.Fatalf("open captures = %v, want only cp_new", stillOpen)
	}
}
