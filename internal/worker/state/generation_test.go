package state_test

import (
	"errors"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

func TestStartGeneration_EachRunTakesTheNextAndItSurvivesReopening(t *testing.T) {
	db, path := open(t)
	for want := uint64(1); want <= 3; want++ {
		if got, err := db.StartGeneration(t.Context()); err != nil || got != want {
			t.Fatalf("run %d: %d, %v", want, got, err)
		}
	}
	_ = db.Close()
	again, err := state.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if got, err := again.StartGeneration(t.Context()); err != nil || got != 4 {
		t.Errorf("after reopening: %d, %v; want 4", got, err)
	}
}

func TestRecoverGeneration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		runs             int // StartGeneration calls; the run under test holds the first
		controller, want uint64
		err              error
	}{
		{name: "a lost file jumps past what the Controller saw", runs: 1, controller: 7, want: 8},
		{name: "a Controller behind the file still gives a new one", runs: 1, controller: 0, want: 2},
		{name: "a later run on the same file supersedes this one", runs: 2, controller: 2, err: state.ErrSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := open(t)
			var held uint64
			for i := range tc.runs {
				g, err := db.StartGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					held = g
				}
			}
			got, err := db.RecoverGeneration(t.Context(), held, tc.controller)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("RecoverGeneration = %d, %v; want %d, %v", got, err, tc.want, tc.err)
			}
			if tc.err != nil {
				return
			}
			if next, err := db.StartGeneration(t.Context()); err != nil || next != tc.want+1 {
				t.Errorf("the next run takes %d, %v; want %d: the recovered one was not stored", next, err, tc.want+1)
			}
		})
	}
}

func TestRecoverGeneration_RefusesAGenerationWithNoRoomAfterIt(t *testing.T) {
	db, _ := open(t)
	held, err := db.StartGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverGeneration(t.Context(), held, 1<<63); err == nil {
		t.Error("a generation that does not fit in the file was taken")
	}
}
