// Package journal is how the Worker's reports reach the Controller: each one
// is written to state.db in the transaction of the change it reports, and sent,
// in order and with the epoch and seq it was written under, until the
// Controller confirms it (§6.4, §20.4).
package journal

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Journal writes the reports of one run of the daemon, which has the epoch
// every row it writes carries.
type Journal struct {
	epoch string
	clock clock.Clock
	wake  chan struct{}
}

// New makes the journal of the daemon with the given epoch.
func New(epoch string, clk clock.Clock) *Journal {
	return &Journal{epoch: epoch, clock: clk, wake: make(chan struct{}, 1)}
}

// Epoch is the epoch of the daemon that writes.
func (j *Journal) Epoch() string { return j.epoch }

// AppendTx adds a report to the transaction that makes the change it tells
// about. The row is not visible, nor worth sending, until the transaction
// commits: the caller calls Notify then. The epoch and seq of the entry are
// those of the row, so the entry is stored without them.
func (j *Journal) AppendTx(ctx context.Context, tx *state.Tx, k state.Key, e v1.RunReportEntry) (uint64, error) {
	e.WorkerEpoch, e.WorkerSeq, e.LeaseEpoch = "", 0, k.LeaseEpoch
	payload, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("journal: encoding a report: %w", err)
	}
	return state.AppendJournal(ctx, tx, state.JournalRow{
		Epoch: j.epoch, Key: k, Kind: string(e.Kind), Payload: payload, CreatedAt: j.clock.Now(),
	})
}

// Notify tells the sender that there is something new to send.
func (j *Journal) Notify() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

func entryOf(r state.JournalRow) (v1.RunReportEntry, error) {
	var e v1.RunReportEntry
	if err := json.Unmarshal(r.Payload, &e); err != nil {
		return e, fmt.Errorf("journal: row %d has a corrupt payload: %w", r.Seq, err)
	}
	e.WorkerEpoch, e.WorkerSeq, e.LeaseEpoch = r.Epoch, r.Seq, r.Key.LeaseEpoch
	return e, nil
}
