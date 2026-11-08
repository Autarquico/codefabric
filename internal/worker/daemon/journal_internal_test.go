package daemon

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

func journaled(t *testing.T, backlog journal.Backlog) (*daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &daemon{stateDB: db, log: slog.New(slog.DiscardHandler), journalBacklog: func() journal.Backlog { return backlog }}, db
}

func seal(t *testing.T, db *state.DB, outputBytes uint64) {
	t.Helper()
	if err := db.WithTx(t.Context(), func(tx *state.Tx) error {
		if err := state.RecordShed(t.Context(), tx, state.Shed{OutputBytes: outputBytes}, time.Unix(100, 0)); err != nil {
			return err
		}
		_, err := state.SealTruncation(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestJournalStatus_AHealthyJournalReportsItsBacklogAndTheTruncationsNotYetAcknowledged(t *testing.T) {
	d, db := journaled(t, journal.Backlog{Bytes: 123})
	seal(t, db, 10)
	seal(t, db, 20)

	report, health := d.journalStatus(t.Context(), v1.Health{Status: v1.HealthOK})

	if health.Status != v1.HealthOK || len(health.Components) != 0 {
		t.Errorf("health = %+v: a journal under its size changes nothing", health)
	}
	if report == nil || report.BacklogBytes != 123 || len(report.Truncations) != 2 || report.Truncations[1].OutputBytes != 20 {
		t.Fatalf("report = %+v", report)
	}

	d.ackJournal(t.Context(), 1)
	report, _ = d.journalStatus(t.Context(), v1.Health{Status: v1.HealthOK})
	if len(report.Truncations) != 1 || report.Truncations[0].ID != 2 {
		t.Fatalf("truncations after the ack of 1 = %+v, want only the second", report.Truncations)
	}
}

func TestJournalStatus_AJournalOverMaxSizeFailsItsComponentAndDegradesTheWorker(t *testing.T) {
	d, _ := journaled(t, journal.Backlog{Bytes: 1 << 30, Reason: "the journal holds too much"})

	_, health := d.journalStatus(t.Context(), v1.Health{Status: v1.HealthOK})

	if health.Status != v1.HealthDegraded || health.Message != "the journal holds too much" {
		t.Errorf("health = %+v", health)
	}
	if len(health.Components) != 1 || health.Components[0].Name != "journal" || health.Components[0].Status != v1.ComponentFail || health.Components[0].Action == "" {
		t.Errorf("components = %+v", health.Components)
	}
}

func TestJournalStatus_ADegradationForAnotherReasonKeepsItsMessage(t *testing.T) {
	d, _ := journaled(t, journal.Backlog{Reason: "journal"})
	other := v1.Health{Status: v1.HealthDegraded, Message: "unknown_command", Components: []v1.ComponentHealth{{Name: "daemon", Status: v1.ComponentFail}}}

	_, health := d.journalStatus(t.Context(), other)

	if health.Message != "unknown_command" || len(health.Components) != 2 {
		t.Errorf("health = %+v", health)
	}
}

func TestJournalStatus_WithoutAJournalThereIsNothingToTell(t *testing.T) {
	report, health := (&daemon{}).journalStatus(t.Context(), v1.Health{Status: v1.HealthOK})
	if report != nil || health.Status != v1.HealthOK {
		t.Errorf("report %+v, health %+v", report, health)
	}
}
