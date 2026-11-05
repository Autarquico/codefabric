package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
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

	mu       sync.Mutex // what Store has not said yet about the lines the cap dropped
	trimmed  int
	warnedAt time.Time
}

const (
	// unknownRouteRetry is how long a Controller older than POST /v1/worker/logs
	// is left alone before it is asked again: it may have been upgraded since.
	unknownRouteRetry = 10 * time.Minute
	// trimWarnEvery bounds how often the cap of the journal says what it dropped.
	trimWarnEvery = time.Minute
	// batchEnvelope is the JSON of a LogBatch around its entries.
	batchEnvelope = len(`{"entries":[]}`)
	truncatedMark = " [truncated]"
)

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
			Time: e.Time, Level: e.Level, Msg: e.Msg, Attrs: e.Attrs, RunID: e.RunID,
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
	l.noteTrimmed(ctx, trimmed)
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return nil
}

// noteTrimmed says what the cap of the journal dropped: at once the first
// time, then at most every trimWarnEvery with what it dropped meanwhile, so
// that a journal that stays full does not double what the Worker writes.
func (l *Logs) noteTrimmed(ctx context.Context, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trimmed += n
	now := l.o.Clock.Now()
	if l.trimmed == 0 || (n > 0 && !l.warnedAt.IsZero() && now.Sub(l.warnedAt) < trimWarnEvery) {
		return
	}
	l.o.Log.WarnContext(ctx, "the log journal is full: its oldest lines were dropped", "dropped", l.trimmed)
	l.trimmed, l.warnedAt = 0, now
}

// Run sends until ctx ends. A Controller that fails is retried with a
// backoff. One that does not know the route (older than this Worker) is said
// so once, at info, and asked again every unknownRouteRetry. The journal
// keeps the lines meanwhile, within its bound.
func (l *Logs) Run(ctx context.Context) error {
	ticker := l.o.Clock.NewTicker(l.o.Tick)
	defer ticker.Stop()
	backoff := time.Duration(0)
	unknownRoute := false
	for {
		sent, err := l.Flush(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, client.ErrUnknownRoute):
			if !unknownRoute {
				l.o.Log.InfoContext(ctx, "this Controller does not take structured logs: the journal keeps the latest lines and asks again later",
					"kept", l.limit(), "again_in", unknownRouteRetry)
				unknownRoute = true
			}
			if !l.wait(ctx, unknownRouteRetry) {
				return nil
			}
			continue
		case err != nil:
			backoff = min(max(backoff*2, backoffInitial), backoffMax)
			l.o.Log.WarnContext(ctx, "sending the logs failed; retrying", "err", err, "in", backoff)
			if !l.wait(ctx, backoff) {
				return nil
			}
			continue
		case sent > 0:
			backoff, unknownRoute = 0, false
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

func (l *Logs) limit() int {
	if l.o.Limit > 0 {
		return l.o.Limit
	}
	return state.MaxLogRows
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
	entries := make([]v1.LogEntry, len(pending))
	sizes := make([]int, len(pending))
	shortened := 0
	for i, r := range pending {
		e, cut := fit(logEntryOf(r))
		if cut {
			shortened++
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return 0, fmt.Errorf("journal: encoding log line %d: %w", r.Seq, err)
		}
		entries[i], sizes[i] = e, len(raw)
	}
	if shortened > 0 {
		l.o.Log.WarnContext(ctx, "log lines too long for the Controller were shortened", "lines", shortened)
	}
	settled := 0
	for len(entries) > 0 {
		n := fitting(sizes)
		batch := entries[:n]
		entries, sizes = entries[n:], sizes[n:]
		if err := l.send(ctx, batch); err != nil {
			var coded *domain.Error
			if errors.Is(err, client.ErrUnknownRoute) || !errors.As(err, &coded) || coded.Code != domain.CodeInvalidRequest {
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

func (l *Logs) send(ctx context.Context, entries []v1.LogEntry) error {
	resp, err := l.o.Shipper.ShipLogs(ctx, v1.LogBatch{Entries: entries})
	if err != nil {
		return err
	}
	if resp.Applied+resp.Duplicate+resp.Rejected != len(entries) {
		return errors.New("journal: the Controller settled another number of log lines than were sent")
	}
	if resp.Rejected > 0 {
		l.o.Log.WarnContext(ctx, "the Controller refused log lines as malformed", "lines", resp.Rejected)
	}
	return nil
}

func logEntryOf(r state.LogRow) v1.LogEntry {
	return v1.LogEntry{
		WorkerEpoch: r.Epoch, WorkerSeq: r.Seq, Timestamp: r.Time, Level: r.Level, Message: r.Msg, Attrs: r.Attrs, RunID: r.RunID,
	}
}

// fit cuts e to the bounds the Controller holds a line to, and says whether it
// had to: a line too long is shortened, and says so, rather than refused.
// Within these bounds a line encodes to far less than v1.LogBatchBytes, even
// with every character escaped, so each fits a request on its own.
func fit(e v1.LogEntry) (v1.LogEntry, bool) {
	cut := false
	if !utf8.ValidString(e.Message) {
		// What JSON would send in place of the invalid bytes, before it is measured.
		e.Message, cut = strings.ToValidUTF8(e.Message, string(utf8.RuneError)), true
	}
	if len(e.Message) > v1.LogMessageMaxBytes {
		e.Message, cut = cutText(e.Message, v1.LogMessageMaxBytes-len(truncatedMark))+truncatedMark, true
	}
	switch {
	case len(e.Attrs) > 0 && !json.Valid(e.Attrs):
		e.Attrs, cut = json.RawMessage(`{"attrs_unencodable":true}`), true
	case len(e.Attrs) > 0:
		// encoding/json sends raw JSON with <, >, &, U+2028 and U+2029 escaped, and the
		// Controller counts the attributes as they arrive: escape them first, so that
		// they are bounded and measured as sent.
		var escaped bytes.Buffer
		json.HTMLEscape(&escaped, e.Attrs)
		e.Attrs = escaped.Bytes()
	}
	switch {
	case len(e.Attrs) > v1.LogAttrsMaxBytes:
		e.Attrs, cut = fmt.Appendf(nil, `{"attrs_truncated_bytes":%d}`, len(e.Attrs)), true
	}
	if len(e.RunID) > v1.LogRunIDMaxLen {
		e.RunID, cut = "", true // no Run has such an ID
	}
	return e, cut
}

// cutText keeps at most n bytes of s, at a character boundary.
func cutText(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// fitting is how many of the first entries, of these encoded sizes, fit one
// request: at most v1.LogBatchMaxEntries, and a body of at most
// v1.LogBatchBytes as sent. What the Controller counts of a batch (its
// messages and attributes, decoded) is never more than that, and the body is
// within v1.LogBodyBytes.
func fitting(sizes []int) int {
	body := batchEnvelope
	for i, n := range sizes {
		if i > 0 {
			body++ // the comma
		}
		body += n
		if i >= v1.LogBatchMaxEntries || (i > 0 && body > v1.LogBatchBytes) {
			return i
		}
	}
	return len(sizes)
}

func seqsOf(entries []v1.LogEntry) []uint64 {
	seqs := make([]uint64, len(entries))
	for i, e := range entries {
		seqs[i] = e.WorkerSeq
	}
	return seqs
}
