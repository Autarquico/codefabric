package state_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

func logLines(t *testing.T, db *state.DB, sizes ...int) {
	t.Helper()
	rows := make([]state.LogRow, len(sizes))
	for i, n := range sizes {
		rows[i] = state.LogRow{Time: t0, Level: "info", Msg: strings.Repeat("m", n), Attrs: []byte(`{}`)}
	}
	tx(t, db, func(x *state.Tx) error {
		_, err := state.AppendLogs(t.Context(), x, "E1", rows, 0)
		return err
	})
}

func TestJournalBacklogCountsBytesNotCharacters(t *testing.T) {
	db, _ := open(t)
	tx(t, db, func(x *state.Tx) error {
		_, err := state.AppendJournal(t.Context(), x, state.JournalRow{
			Epoch: "E1", Key: state.Key{RunID: "run_1", LeaseEpoch: 1}, Kind: "started", Payload: []byte(`{"a":"ñ"}`), CreatedAt: t0,
		})
		return err
	})
	logLines(t, db, 10)

	b, err := state.JournalBacklog(t.Context(), db.Reader())
	if err != nil {
		t.Fatal(err)
	}
	if b.Reports != 10 { // {"a":"ñ"} is 9 characters and 10 bytes
		t.Errorf("reports = %d bytes, want 10", b.Reports)
	}
	if b.Logs != 12 { // the message and the {} of the attributes
		t.Errorf("logs = %d bytes, want 12", b.Logs)
	}
}

func TestShedLogsDeletesTheOldestAndRecordsTheRange(t *testing.T) {
	db, _ := open(t)
	logLines(t, db, 100, 100, 100, 100)

	var freed int64
	tx(t, db, func(x *state.Tx) (err error) {
		freed, err = state.ShedLogs(t.Context(), x, 150, t0)
		return err
	})
	if freed != 204 { // two lines of 102 bytes: enough to free 150
		t.Fatalf("freed %d, want 204", freed)
	}
	left, err := state.PendingLogs(t.Context(), db.Reader(), 10)
	if err != nil || len(left) != 2 || left[0].Seq != 3 {
		t.Fatalf("left = %+v (%v), want lines 3 and 4", left, err)
	}
	// Not sealed: nothing is reported until the journal reaches hard_max.
	if sealed, err := state.SealedTruncations(t.Context(), db.Reader(), 10); err != nil || len(sealed) != 0 {
		t.Fatalf("sealed = %+v (%v)", sealed, err)
	}
	tx(t, db, func(x *state.Tx) error {
		_, err := state.SealTruncation(t.Context(), x)
		return err
	})
	sealed, err := state.SealedTruncations(t.Context(), db.Reader(), 10)
	if err != nil || len(sealed) != 1 {
		t.Fatalf("sealed = %+v (%v)", sealed, err)
	}
	if got := sealed[0]; got.LogsFrom != 1 || got.LogsTo != 2 || got.LogLines != 2 || got.OutputBytes != 0 {
		t.Errorf("truncation = %+v, want lines 1..2", got)
	}
}

func TestShedsAccumulateInTheOpenTruncationAndItsRangeWidens(t *testing.T) {
	db, _ := open(t)
	logLines(t, db, 10, 10, 10)
	shed := func(need int64, at time.Time) {
		tx(t, db, func(x *state.Tx) error {
			_, err := state.ShedLogs(t.Context(), x, need, at)
			return err
		})
	}
	shed(1, t0)
	tx(t, db, func(x *state.Tx) error {
		return state.RecordShed(t.Context(), x, state.Shed{OutputBytes: 500}, t0.Add(time.Second))
	})
	shed(1, t0.Add(2*time.Second))
	tx(t, db, func(x *state.Tx) error {
		_, err := state.SealTruncation(t.Context(), x)
		return err
	})

	sealed, err := state.SealedTruncations(t.Context(), db.Reader(), 10)
	if err != nil || len(sealed) != 1 {
		t.Fatalf("sealed = %+v (%v), want one", sealed, err)
	}
	got := sealed[0]
	if got.LogsFrom != 1 || got.LogsTo != 2 || got.LogLines != 2 || got.OutputBytes != 500 || !got.At.Equal(t0.Add(2*time.Second)) {
		t.Errorf("truncation = %+v", got)
	}
}

func TestAnOutputOnlyShedHasNoLogRangeAndALaterOneDoesNotStartItAtZero(t *testing.T) {
	db, _ := open(t)
	tx(t, db, func(x *state.Tx) error { return state.RecordShed(t.Context(), x, state.Shed{OutputBytes: 5}, t0) })
	logLines(t, db, 10)
	tx(t, db, func(x *state.Tx) error {
		_, err := state.ShedLogs(t.Context(), x, 1, t0)
		return err
	})
	tx(t, db, func(x *state.Tx) error {
		_, err := state.SealTruncation(t.Context(), x)
		return err
	})
	sealed, _ := state.SealedTruncations(t.Context(), db.Reader(), 10)
	if len(sealed) != 1 || sealed[0].LogsFrom != 1 || sealed[0].LogsTo != 1 {
		t.Fatalf("sealed = %+v, want the range 1..1", sealed)
	}
}

func TestTruncationsAreAcknowledgedUpToAnIDAndIDsAreNeverReused(t *testing.T) {
	db, _ := open(t)
	seal := func() {
		tx(t, db, func(x *state.Tx) error {
			if err := state.RecordShed(t.Context(), x, state.Shed{OutputBytes: 1}, t0); err != nil {
				return err
			}
			_, err := state.SealTruncation(t.Context(), x)
			return err
		})
	}
	seal()
	seal()
	seal()
	tx(t, db, func(x *state.Tx) error { return state.AckTruncations(t.Context(), x, 2) })
	seal()

	got, err := state.SealedTruncations(t.Context(), db.Reader(), 10)
	if err != nil || len(got) != 2 || got[0].ID != 3 || got[1].ID != 4 {
		t.Fatalf("truncations = %+v (%v), want ids 3 and 4", got, err)
	}
}
