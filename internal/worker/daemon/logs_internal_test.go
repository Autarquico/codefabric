package daemon

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// What the daemon logs reaches the log journal, redacted of the values its
// Runs hold, and is still there once the tee has stopped. What the journal
// says about storing lines is not stored.
func TestTheLogsOfTheWorkerAreJournaledRedactedOfTheValuesOfItsRuns(t *testing.T) {
	const value = "delivered-value-0123456789"
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})), clock: clock.NewFake(time.Unix(0, 0)), epoch: "EPOCH"}
	_, held, stop := d.teeLogs(ctx, db, nil)
	if err := held.Hold("run-dir", []redact.Needle{{Ref: "team/token", Value: []byte(value)}}); err != nil {
		t.Fatal(err)
	}
	// A failure on a path that formats an error without scrubbing it.
	d.log.ErrorContext(ctx, "rendering the configuration failed: "+value, "run", "run_01", "err", errors.New(`exit 1: `+value))
	stop()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := state.PendingLogs(ctx, db.Reader(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d lines in the journal: %+v", len(rows), rows)
	}
	r := rows[0]
	if strings.Contains(r.Msg, value) || strings.Contains(string(r.Attrs), value) ||
		!strings.Contains(r.Msg, "[REDACTED:team/token]") || !strings.Contains(string(r.Attrs), "[REDACTED:team/token]") {
		t.Fatalf("journaled line = %q %s", r.Msg, r.Attrs)
	}
	if r.RunID != "run_01" || r.Epoch != "EPOCH" {
		t.Fatalf("journaled line = %+v", r)
	}
}
