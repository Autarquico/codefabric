package store

import (
	"errors"
	"path/filepath"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const sealed1 = "1111111111111111111111111111111111111111"

// exitedRun is a Run whose harness exited under an accepted lease: terminal,
// publication pending, no result yet.
func (f *leaseFixture) exitedRun(n int) (Run, Lease) {
	f.t.Helper()
	ctx := f.t.Context()
	run := f.queuedRun(n)
	l := f.assign(run, f.alpha, v1.Capacity{CPU: 1})
	run = f.run(run.ID)
	f.tx(func(tx *Tx) error {
		if err := tx.AcceptLease(ctx, l.ID, epoch0); err != nil {
			return err
		}
		if err := tx.AdvanceRun(ctx, run, domain.RunPreparing); err != nil {
			return err
		}
		run.State = domain.RunPreparing
		if err := tx.AdvanceRun(ctx, run, domain.RunRunning); err != nil {
			return err
		}
		run.State = domain.RunRunning
		return tx.ExitRun(ctx, run, domain.RunCompleted, domain.EndExitCode, nil, epoch0)
	})
	return f.run(run.ID), l
}

func TestRuns_CompletedWithPublicationNoneIsRefusedByTheDatabase(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, _ := f.exitedRun(1)
	if run.State != domain.RunCompleted || run.Publication != domain.PublicationPending {
		t.Fatalf("exit left the Run %s/%s", run.State, run.Publication)
	}
	err := f.db.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runs SET publication = 'none' WHERE id = ?`, string(run.ID))
		return err
	})
	if err == nil {
		t.Error("a completed Run went back to publication none")
	}
	bad := Run{
		ID: "run_z", Short: 99, TaskID: "task_1", Attempt: 9, Lineage: domain.LineageInitial, Spec: []byte(`{}`), SpecDigest: "d",
		SourceCommit: "c", State: domain.RunCompleted, Publication: domain.PublicationNone,
		Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch0,
	}
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.InsertRun(ctx, bad) }); err == nil {
		t.Error("a completed Run with publication none was inserted")
	}
}

func TestPublication_EveryChangeIsACompareAndSetOnOneRow(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, _ := f.exitedRun(1)

	// A different commit cannot be sealed over the first, and the first cannot be sealed twice.
	f.tx(func(tx *Tx) error { return tx.SealRun(ctx, run, sealed1, []string{"a.env"}, true) })
	sealed := f.run(run.ID)
	if sealed.ResultCommit != sealed1 || len(sealed.ResultExcluded) != 1 || !sealed.ResultExcludedTruncated {
		t.Fatalf("sealed run = %+v", sealed)
	}
	for name, fn := range map[string]func(tx *Tx) error{
		"seal again":                 func(tx *Tx) error { return tx.SealRun(ctx, sealed, "2222222222222222222222222222222222222222", nil, false) },
		"publish another commit":     func(tx *Tx) error { return tx.PublishRun(ctx, sealed, "2222222222222222222222222222222222222222") },
		"retry what is not failed":   func(tx *Tx) error { return tx.RetryPublication(ctx, sealed) },
		"fail a stale result commit": func(tx *Tx) error { stale := sealed; stale.ResultCommit = ""; return tx.FailPublication(ctx, stale) },
		"fail under another lease":   func(tx *Tx) error { stale := sealed; stale.LeaseEpoch++; return tx.FailPublication(ctx, stale) },
	} {
		if err := f.db.WithTx(ctx, fn); !errors.Is(err, ErrNoRowChanged) {
			t.Errorf("%s: %v, want ErrNoRowChanged", name, err)
		}
	}

	f.tx(func(tx *Tx) error { return tx.FailPublication(ctx, sealed) })
	failed := f.run(run.ID)
	if failed.Publication != domain.PublicationFailed {
		t.Fatalf("publication = %s", failed.Publication)
	}
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.PublishRun(ctx, failed, sealed1) }); !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("publishing a failed publication: %v", err)
	}
	f.tx(func(tx *Tx) error { return tx.RetryPublication(ctx, failed) })
	f.tx(func(tx *Tx) error { return tx.PublishRun(ctx, f.run(run.ID), sealed1) })
	if got := f.run(run.ID).Publication; got != domain.PublicationPublished {
		t.Errorf("publication = %s", got)
	}
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.FailPublication(ctx, f.run(run.ID)) }); !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("a published result was failed: %v", err)
	}
}

func TestCommands_OnlyOnePublishOrderWaitsPerRun(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, l := f.exitedRun(1)
	order := WorkerCommand{WorkerID: f.alpha.ID, Type: domain.CommandPublish, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch, Payload: []byte(`{}`), CreatedAt: epoch0}
	f.tx(func(tx *Tx) error { _, err := tx.EnqueueCommand(ctx, order); return err })
	if err := f.db.WithTx(ctx, func(tx *Tx) error { _, err := tx.EnqueueCommand(ctx, order); return err }); err == nil {
		t.Error("two publish orders wait for the same Run")
	}
	f.tx(func(tx *Tx) error { _, err := tx.AckCommandsThrough(ctx, f.alpha.ID, 100, epoch0); return err })
	f.tx(func(tx *Tx) error { _, err := tx.EnqueueCommand(ctx, order); return err })
}

func TestOpen_UpgradesFrom0104WithLiveAndTerminalRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 104)
	db, err := Open(t.Context(), path, Options{Clock: clock.NewFake(epoch0)})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var run Run
	if err := db.Read(t.Context(), func(q Querier) (err error) {
		run, err = RunByID(t.Context(), q, "run_1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if run.Branch != "" || run.ResultCommit != "" || run.ResultExcluded != nil {
		t.Errorf("a Run from before the migration has %q, %q, %v", run.Branch, run.ResultCommit, run.ResultExcluded)
	}
}
