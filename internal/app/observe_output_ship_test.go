package app_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/outputship"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// controllerOf is the real Controller service as the shipper's Uploader.
type controllerOf struct {
	svc *runs.Service
	by  pki.Session
}

func (c controllerOf) UploadOutput(ctx context.Context, b v1.OutputBatch) (v1.OutputBatchResponse, error) {
	return c.svc.AppendOutput(ctx, c.by, b)
}

// A Worker with a real outputship and a real Controller (service, store, sealer
// and CAS): more than three MiB of output arrive whole and in order across
// sealed segments and the hot tail. Backpressure (CF_IN_PROGRESS, from 8 MiB
// waiting to be sealed) is covered with the fake Controller in outputship.
func TestOutput_AWorkerShipsMoreThanThreeMiBAndTheControllerHoldsAllOfItInOrder(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	ctx := t.Context()

	root := t.TempDir()
	paths := config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")}
	db, err := state.Open(ctx, paths.WorkerStateFile())
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	key := state.Key{RunID: o.run.ID, LeaseEpoch: o.lease.Epoch}
	must(t, db.WithTx(ctx, func(tx *state.Tx) error {
		r := state.Run{
			Key: key, LeaseID: "lease_x", RunShort: o.run.Short, TaskID: "task_1", ProjectID: "project_1", CommandSeq: 1, AcceptBy: at.Add(time.Minute),
			SourceCommit: "abc", CPU: 1, MemoryMB: 1, Spec: []byte(`{}`), Harness: "command", Mode: "headless", CreatedAt: at,
		}
		if _, err := state.InsertAssigned(ctx, tx, r); err != nil {
			return err
		}
		if err := state.Advance(ctx, tx, key, state.PhaseAssigned, state.PhaseAccepted, at); err != nil {
			return err
		}
		if err := state.Advance(ctx, tx, key, state.PhaseAccepted, state.PhaseStarting, at); err != nil {
			return err
		}
		return state.SetRunning(ctx, tx, key, `{"runtime":"tmux","ref":"x/y"}`, at)
	}))

	// A log whose pattern repeats every 182 bytes: the byte compare with the Start/End
	// checks is what shows a reordering or a hole.
	log := make([]byte, 3<<20+4321)
	for i := range log {
		log[i] = byte('a' + (i/7)%26)
	}
	must(t, os.MkdirAll(paths.WorkerRunDir(o.run.ID, o.lease.Epoch), 0o700))
	must(t, os.WriteFile(filepath.Join(paths.WorkerRunDir(o.run.ID, o.lease.Epoch), execwrap.OutputLog), log, 0o600))

	clk := clock.NewFake(at)
	ship := outputship.New(outputship.Options{
		DB: db, Uploader: controllerOf{e.app.Runs, o.worker}, Paths: paths, Clock: clk, Log: slog.New(slog.DiscardHandler),
	})
	for range 400 {
		cur, err := state.OutputCursorOf(ctx, db.Reader(), key)
		must(t, err)
		if cur.Sent == uint64(len(log)) {
			break
		}
		clk.Advance(time.Second)
		must(t, ship.Once(ctx))
		time.Sleep(time.Millisecond) // the sealer is a goroutine of the Controller
	}
	if cur, err := state.OutputCursorOf(ctx, db.Reader(), key); err != nil || cur.Sent != uint64(len(log)) {
		t.Fatalf("the cursor is at %d of %d (err %v)", cur.Sent, len(log), err)
	}

	var got []byte
	var from uint64
	for from < uint64(len(log)) {
		out := e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: from, Limit: 256 << 10})
		if out.Truncated || out.Offset != from || len(out.Data) == 0 {
			t.Fatalf("read at %d: %+v", from, out)
		}
		got = append(got, out.Data...)
		from += uint64(len(out.Data))
	}
	if !bytes.Equal(got, log) {
		t.Fatalf("the Controller holds %d bytes that are not the %d of the log", len(got), len(log))
	}
	if out := e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 0, Limit: 16}); out.Start != 0 || out.End != uint64(len(log)) {
		t.Fatalf("the Controller holds [%d, %d)", out.Start, out.End)
	}
}
