package journal

import (
	"context"
	"errors"
	"log/slog"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// LogShipper is the Controller as the log sender sees it.
type LogShipper interface {
	ShipLogs(ctx context.Context, batch v1.LogBatch) (v1.LogBatchResponse, error)
}

// LogsOptions parameterise NewLogs.
type LogsOptions struct {
	DB      *state.DB
	Epoch   string // of this run of the daemon
	Shipper LogShipper
	Clock   clock.Clock
	// Log is where the log journal says what goes wrong with it. It must not be
	// the teed logger: what it says about storing or sending lines would be
	// another line to store and send.
	Log   *slog.Logger
	Tick  time.Duration // 0 means a second
	Limit int           // rows the journal keeps; 0 means state.MaxLogRows
}

// Logs is the journal of the structured logs of the Worker (§20.4, §34): Store
// is the sink of its slogtee, which writes the lines to state.db, and Run
// sends them, in the order of their seq, until the Controller has them. Unlike
// the reports, nothing about them is fenced: the Controller settles every
// line it receives, stored, duplicate or malformed.
type Logs struct {
	o    LogsOptions
	wake chan struct{}
}

// NewLogs makes the log journal.
func NewLogs(o LogsOptions) *Logs {
	if o.Tick <= 0 {
		o.Tick = defaultTick
	}
	return &Logs{o: o, wake: make(chan struct{}, 1)}
}

// Store writes entries to the journal in one transaction, and wakes the sender.
func (l *Logs) Store(ctx context.Context, entries []slogtee.Entry) error {
	rows := make([]state.LogRow, len(entries))
	for i, e := range entries {
		rows[i] = state.LogRow{
			Time: e.Time, Level: e.Level, Msg: e.Msg, Attrs: e.Attrs, RunID: e.RunID, TaskID: e.TaskID, ProjectID: e.ProjectID,
		}
	}
	var trimmed int
	err := l.o.DB.WithTx(ctx, func(tx *state.Tx) (err error) {
		trimmed, err = state.AppendLogs(ctx, tx, l.o.Epoch, rows, l.o.Limit)
		return err
	})
	if err != nil {
		return err
	}
	if trimmed > 0 {
		l.o.Log.WarnContext(ctx, "the log journal is full: its oldest lines were dropped", "dropped", trimmed)
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return nil
}

// Run sends until ctx ends. A Controller that fails, or that does not know
// the route (one older than this Worker), is retried with a backoff; the
// journal keeps the lines meanwhile, within its bound.
func (l *Logs) Run(ctx context.Context) error {
	ticker := l.o.Clock.NewTicker(l.o.Tick)
	defer ticker.Stop()
	backoff := time.Duration(0)
	for {
		sent, err := l.Flush(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			backoff = min(max(backoff*2, backoffInitial), backoffMax)
			l.o.Log.WarnContext(ctx, "sending the logs failed; retrying", "err", err, "in", backoff)
			if !l.wait(ctx, backoff) {
				return nil
			}
			continue
		case sent > 0:
			backoff = 0
			continue
		}
		backoff = 0
		select {
		case <-ctx.Done():
			return nil
		case <-l.wake:
		case <-ticker.C():
		}
	}
}

func (l *Logs) wait(ctx context.Context, d time.Duration) bool {
	timer := l.o.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}

// Flush sends a page of the journal and returns how many lines the Controller
// settled. An error leaves what was not settled.
func (l *Logs) Flush(ctx context.Context) (int, error) {
	pending, err := state.PendingLogs(ctx, l.o.DB.Reader(), pageRows)
	if err != nil {
		return 0, err
	}
	settled := 0
	for len(pending) > 0 {
		batch, rest := splitLogs(pending)
		pending = rest
		if err := l.send(ctx, batch); err != nil {
			var coded *domain.Error
			if !errors.As(err, &coded) || coded.Code != domain.CodeInvalidRequest {
				return settled, err
			}
			// The Controller refuses the batch as it is, and will every time: it goes.
			l.o.Log.WarnContext(ctx, "the Controller refused a batch of logs; it is dropped", "lines", len(batch), "message", coded.Message)
		}
		if err := l.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.DeleteLogs(ctx, tx, seqsOf(batch)) }); err != nil {
			return settled, err
		}
		settled += len(batch)
	}
	return settled, nil
}

func (l *Logs) send(ctx context.Context, rows []state.LogRow) error {
	b := v1.LogBatch{Entries: make([]v1.LogEntry, len(rows))}
	for i, r := range rows {
		b.Entries[i] = v1.LogEntry{
			WorkerEpoch: r.Epoch, WorkerSeq: r.Seq, Timestamp: r.Time, Level: r.Level, Message: r.Msg, Attrs: r.Attrs,
			RunID: r.RunID, TaskID: r.TaskID, ProjectID: r.ProjectID,
		}
	}
	resp, err := l.o.Shipper.ShipLogs(ctx, b)
	if err != nil {
		return err
	}
	if resp.Applied+resp.Duplicate+resp.Rejected != len(rows) {
		return errors.New("journal: the Controller settled another number of log lines than were sent")
	}
	if resp.Rejected > 0 {
		l.o.Log.WarnContext(ctx, "the Controller refused log lines as malformed", "lines", resp.Rejected)
	}
	return nil
}

// splitLogs takes the longest prefix that fits one request.
func splitLogs(rows []state.LogRow) (batch, rest []state.LogRow) {
	size := 0
	for i, r := range rows {
		size += len(r.Msg) + len(r.Attrs)
		if i >= v1.LogBatchMaxEntries || (i > 0 && size > v1.LogBatchBytes) {
			return rows[:i], rows[i:]
		}
	}
	return rows, nil
}

func seqsOf(rows []state.LogRow) []uint64 {
	seqs := make([]uint64, len(rows))
	for i, r := range rows {
		seqs[i] = r.Seq
	}
	return seqs
}
