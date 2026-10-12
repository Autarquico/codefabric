package state_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

const (
	parent  = "1111111111111111111111111111111111111111"
	result  = "2222222222222222222222222222222222222222"
	result2 = "3333333333333333333333333333333333333333"
)

func intent(id string) state.Publication {
	return state.Publication{Key: state.Key{RunID: id, LeaseEpoch: 1}, ParentOID: parent, SealedAt: t0.Add(-time.Minute), CreatedAt: t0}
}

func read(t *testing.T, q state.Querier, k state.Key) state.Publication {
	t.Helper()
	p, err := state.PublicationOf(t.Context(), q, k)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublication_TheIntentIsWrittenOnceAndSurvivesReopening(t *testing.T) {
	db, path := open(t)
	p := intent("run_a")
	tx(t, db, func(x *state.Tx) error {
		ok, err := state.InsertPublication(t.Context(), x, p)
		if !ok || err != nil {
			t.Fatalf("first insert = %v, %v", ok, err)
		}
		other := p
		other.ParentOID, other.SealedAt = result, t0
		if ok, err := state.InsertPublication(t.Context(), x, other); ok || err != nil {
			t.Errorf("the intent was overwritten: %v, %v", ok, err)
		}
		return nil
	})
	_ = db.Close()
	db2, err := state.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	got := read(t, db2.Reader(), p.Key)
	if got.State != state.PubSealing || got.ParentOID != parent || !got.SealedAt.Equal(p.SealedAt) || got.Generation != 1 {
		t.Errorf("publication = %+v", got)
	}
	if _, err := state.PublicationOf(t.Context(), db2.Reader(), state.Key{RunID: "run_x", LeaseEpoch: 1}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown publication: %v", err)
	}
}

func TestPublication_TheStepsAreCompareAndSetsOnGenerationAndAttempt(t *testing.T) {
	db, _ := open(t)
	p := intent("run_a")
	tx(t, db, func(x *state.Tx) error { _, err := state.InsertPublication(t.Context(), x, p); return err })

	tx(t, db, func(x *state.Tx) error {
		return state.MarkSealed(t.Context(), x, read(t, x, p.Key), result, []string{"a", "b"}, true, t0)
	})
	sealed := read(t, db.Reader(), p.Key)
	if sealed.State != state.PubSealed || sealed.ResultCommit != result || len(sealed.Excluded) != 2 || !sealed.ExcludedTruncated {
		t.Fatalf("sealed = %+v", sealed)
	}
	// Sealing twice does not match: it is no longer sealing.
	if err := db.WithTx(t.Context(), func(x *state.Tx) error { return state.MarkSealed(t.Context(), x, sealed, result2, nil, false, t0) }); !errors.Is(err, state.ErrChanged) {
		t.Errorf("second seal: %v", err)
	}

	// A push of an old attempt cannot mark the newer one.
	var attempts int
	tx(t, db, func(x *state.Tx) (err error) {
		attempts, err = state.Retry(t.Context(), x, sealed, true, t0.Add(time.Minute), "refused", t0)
		return err
	})
	if attempts != 1 {
		t.Errorf("attempts = %d", attempts)
	}
	if err := db.WithTx(t.Context(), func(x *state.Tx) error { return state.MarkPushed(t.Context(), x, sealed, t0) }); !errors.Is(err, state.ErrChanged) {
		t.Errorf("the push of an old attempt: %v", err)
	}
	fresh := read(t, db.Reader(), p.Key)
	tx(t, db, func(x *state.Tx) error { return state.MarkPushed(t.Context(), x, fresh, t0) })
	if got := read(t, db.Reader(), p.Key).State; got != state.PubPushed {
		t.Fatalf("state = %s", got)
	}
	// Unreachable does not count.
	pushed := read(t, db.Reader(), p.Key)
	if err := db.WithTx(t.Context(), func(x *state.Tx) error { return state.FailPublication(t.Context(), x, sealed, "x", t0) }); !errors.Is(err, state.ErrChanged) {
		t.Errorf("failing with an old state: %v", err)
	}

	ok := false
	tx(t, db, func(x *state.Tx) (err error) {
		ok, err = state.ConfirmPublication(t.Context(), x, p.Key, state.PubPublished, t0)
		return err
	})
	if !ok || read(t, db.Reader(), p.Key).State != state.PubPublished {
		t.Fatal("not confirmed")
	}
	tx(t, db, func(x *state.Tx) (err error) {
		ok, err = state.ConfirmPublication(t.Context(), x, p.Key, state.PubFailed, t0)
		return err
	})
	if ok {
		t.Error("a published publication was confirmed again")
	}
	_ = pushed
}

func TestPublication_RequeueKeepsTheCommitAndAnOldTryCannotOverwriteTheNewOne(t *testing.T) {
	db, _ := open(t)
	p := intent("run_a")
	tx(t, db, func(x *state.Tx) error {
		if _, err := state.InsertPublication(t.Context(), x, p); err != nil {
			return err
		}
		return state.MarkSealed(t.Context(), x, read(t, x, p.Key), result, nil, false, t0)
	})
	old := read(t, db.Reader(), p.Key)
	tx(t, db, func(x *state.Tx) error { return state.FailPublication(t.Context(), x, old, "refused", t0) })
	var requeued bool
	tx(t, db, func(x *state.Tx) (err error) {
		requeued, err = state.RequeuePublication(t.Context(), x, p.Key, t0.Add(time.Hour))
		return err
	})
	got := read(t, db.Reader(), p.Key)
	if !requeued || got.State != state.PubSealed || got.ResultCommit != result || got.Generation != 2 || got.Attempts != 0 {
		t.Fatalf("requeued = %v %+v", requeued, got)
	}
	// The attempt of the old try that was running meanwhile reports back: it changes nothing.
	for name, fn := range map[string]func(x *state.Tx) error{
		"push":  func(x *state.Tx) error { return state.MarkPushed(t.Context(), x, old, t0) },
		"fail":  func(x *state.Tx) error { return state.FailPublication(t.Context(), x, old, "late", t0) },
		"retry": func(x *state.Tx) error { _, err := state.Retry(t.Context(), x, old, true, t0, "late", t0); return err },
	} {
		if err := db.WithTx(t.Context(), fn); !errors.Is(err, state.ErrChanged) {
			t.Errorf("%s of an old try: %v", name, err)
		}
	}
	// Requeueing a publication that is trying does nothing; one with no commit seals again.
	tx(t, db, func(x *state.Tx) (err error) {
		requeued, err = state.RequeuePublication(t.Context(), x, p.Key, t0)
		return err
	})
	if requeued {
		t.Error("a publication that is trying was requeued")
	}
	q := intent("run_b")
	tx(t, db, func(x *state.Tx) error {
		if _, err := state.InsertPublication(t.Context(), x, q); err != nil {
			return err
		}
		return state.FailPublication(t.Context(), x, read(t, x, q.Key), "no space", t0)
	})
	tx(t, db, func(x *state.Tx) (err error) {
		requeued, err = state.RequeuePublication(t.Context(), x, q.Key, t0)
		return err
	})
	if got := read(t, db.Reader(), q.Key); !requeued || got.State != state.PubSealing {
		t.Errorf("a failed sealing = %+v", got)
	}
}

func TestPublication_DueAndPending(t *testing.T) {
	db, _ := open(t)
	a, b, c := intent("run_a"), intent("run_b"), intent("run_c")
	tx(t, db, func(x *state.Tx) error {
		for _, p := range []state.Publication{a, b, c} {
			if _, err := state.InsertPublication(t.Context(), x, p); err != nil {
				return err
			}
		}
		if err := state.MarkSealed(t.Context(), x, read(t, x, b.Key), result, nil, false, t0.Add(time.Hour)); err != nil {
			return err
		}
		return state.FailPublication(t.Context(), x, read(t, x, c.Key), "x", t0)
	})
	due, err := state.DuePublications(t.Context(), db.Reader(), t0, 10)
	if err != nil || len(due) != 1 || due[0].RunID != "run_a" {
		t.Fatalf("due at t0 = %+v (%v)", due, err)
	}
	if due, _ = state.DuePublications(t.Context(), db.Reader(), t0.Add(2*time.Hour), 10); len(due) != 2 {
		t.Errorf("due later = %d", len(due))
	}
	pending, err := state.PendingPublications(t.Context(), db.Reader())
	if err != nil || len(pending) != 2 {
		t.Errorf("pending = %+v (%v)", pending, err)
	}
	next, ok, err := state.NextPublicationDue(t.Context(), db.Reader())
	if err != nil || !ok || !next.Equal(t0) {
		t.Errorf("next = %v %v %v", next, ok, err)
	}
}

func TestAssignmentKeepsTheBranchAndTheRepo(t *testing.T) {
	db, _ := open(t)
	a := assignment("run_a", 1, 1)
	a.Branch, a.Repo = "cf/x/run-1", "/srv/r"
	tx(t, db, func(x *state.Tx) error { _, err := state.InsertAssigned(t.Context(), x, a); return err })
	got, err := state.RunBy(t.Context(), db.Reader(), a.Key)
	if err != nil || got.Branch != "cf/x/run-1" || got.Repo != "/srv/r" {
		t.Errorf("run = %+v (%v)", got, err)
	}
}
