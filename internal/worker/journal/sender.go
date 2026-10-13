package journal

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Reporter is the Controller as the sender sees it.
type Reporter interface {
	ReportRun(ctx context.Context, run string, req v1.RunReportRequest) (v1.RunReportResponse, error)
}

const (
	pageRows       = 256
	defaultTick    = time.Second
	backoffInitial = time.Second
	backoffMax     = 30 * time.Second
)

// Sender sends the journal. Rows go in the order of their seq, by Run, up to
// v1.MaxReportEntries and v1.MaxReportBytes per request, and are deleted only
// once the Controller has them (applied or duplicate).
//
// A rejection of an entry (stale lease, unknown lease epoch, a transition the Run
// no longer allows) fences the Run: the sender stops sending its reports and
// keeps them, so that the reconciliation of the Controller decides what they are
// worth. So does a request the Controller refuses as malformed or for a Run it
// does not know (CF_INVALID_REQUEST, CF_NOT_FOUND): it would be refused every
// time, and only that Run is affected. Any other error of the whole request (no
// connection, authentication, a server that fails) says nothing of the entries
// and fences nothing: it is retried with a backoff and loses nothing.
type Sender struct {
	DB       *state.DB
	Reporter Reporter
	Journal  *Journal
	Clock    clock.Clock
	Log      *slog.Logger
	Tick     time.Duration // 0 means a second
	// OnView, if set, is called for each request the Controller applied, with the Run
	// as the Controller shows it and the seqs of the reports it settled, in the transaction that deletes the confirmed reports.
	// An error rolls that transaction back and the reports are sent again.
	OnView func(ctx context.Context, tx *state.Tx, k state.Key, confirmed []uint64, run v1.Run) error
}

// Run sends until ctx ends.
func (s *Sender) Run(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	ticker := s.Clock.NewTicker(tick)
	defer ticker.Stop()
	backoff := time.Duration(0)
	for {
		sent, err := s.Flush(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			backoff = min(max(backoff*2, backoffInitial), backoffMax)
			s.Log.WarnContext(ctx, "sending reports failed; retrying", "err", err, "in", backoff)
			if !s.wait(ctx, backoff) {
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
		case <-s.Journal.wake:
		case <-ticker.C():
		}
	}
}

func (s *Sender) wait(ctx context.Context, d time.Duration) bool {
	timer := s.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}

type group struct {
	key  state.Key
	rows []state.JournalRow
}

// Flush sends what is pending once and returns how many rows the Controller
// settled (confirmed, or refused for good). An error means the Controller could
// not be reached or answered something unexpected: what was not settled stays.
func (s *Sender) Flush(ctx context.Context) (int, error) {
	pending, err := state.PendingJournal(ctx, s.DB.Reader(), pageRows)
	if err != nil {
		return 0, err
	}
	var groups []*group
	index := map[state.Key]*group{}
	for _, r := range pending {
		g, ok := index[r.Key]
		if !ok {
			g = &group{key: r.Key}
			index[r.Key], groups = g, append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	settled := 0
	var first error
	for _, g := range groups {
		for len(g.rows) > 0 {
			batch, rest := split(g.rows)
			g.rows = rest
			n, fenced, err := s.send(ctx, g.key, batch)
			settled += n
			if err != nil {
				var coded *domain.Error
				if errors.As(err, &coded) && (coded.Code == domain.CodeInvalidRequest || coded.Code == domain.CodeNotFound) {
					// The Controller refuses these reports as they are, and will every time: for
					// this Run alone that is final. Nothing about the other Runs, and no backoff.
					if fenceErr := s.fence(ctx, g.key, coded.Code, nil, coded.Message); fenceErr != nil {
						first = cmp.Or(first, fenceErr)
					}
					break
				}
				// Any other error of the request (network, authentication, a server that
				// fails) says nothing of the entries: this Run waits, the other Runs go on,
				// and the sender backs off.
				first = cmp.Or(first, err)
				break
			}
			if fenced {
				break
			}
		}
	}
	return settled, first
}

// split takes the longest prefix that fits one request.
func split(rows []state.JournalRow) (batch, rest []state.JournalRow) {
	size := 64 // the envelope
	for i, r := range rows {
		size += len(r.Payload) + len(r.Epoch) + 80 // the epoch, seq and lease epoch the payload lacks
		if i >= v1.MaxReportEntries || (i > 0 && size > v1.MaxReportBytes) {
			return rows[:i], rows[i:]
		}
	}
	return rows, nil
}

// send makes one request. It returns how many rows are settled and whether the
// Run was fenced.
func (s *Sender) send(ctx context.Context, k state.Key, batch []state.JournalRow) (settled int, fenced bool, err error) {
	req := v1.RunReportRequest{Entries: make([]v1.RunReportEntry, 0, len(batch))}
	for _, r := range batch {
		e, err := entryOf(r)
		if err != nil {
			return 0, false, err
		}
		req.Entries = append(req.Entries, e)
	}
	resp, err := s.Reporter.ReportRun(ctx, k.RunID, req)
	if err != nil {
		return 0, false, err
	}
	if len(resp.Outcomes) != len(batch) {
		return 0, false, errors.New("journal: the Controller answered another number of outcomes than entries")
	}
	var done []uint64
	var refusal *v1.RunReportOutcome
	for i, o := range resp.Outcomes {
		if o.WorkerSeq != batch[i].Seq {
			return 0, false, errors.New("journal: the outcomes are not those of the entries sent")
		}
		switch o.Result {
		case v1.ReportApplied, v1.ReportDuplicate:
			done = append(done, o.WorkerSeq)
		case v1.ReportRejected:
			if v1.RunReportKind(batch[i].Kind) == v1.ReportOrphanPreserved {
				// The one report of an orphan is not behind a fence: refused, it is
				// final and goes, or it would be sent again for ever.
				s.Log.WarnContext(ctx, "the Controller refused the preservation of an orphan; it is not sent again",
					"run", k.RunID, "lease_epoch", k.LeaseEpoch, "code", o.Code, "message", o.Message)
				done = append(done, o.WorkerSeq)
				continue
			}
			refusal = &resp.Outcomes[i]
		case v1.ReportNotApplied:
		default:
			return 0, false, errors.New("journal: the Controller answered an outcome this Worker does not know")
		}
		if refusal != nil {
			break
		}
	}
	if refusal != nil {
		return len(done), true, s.fence(ctx, k, refusal.Code, done, refusal.Message)
	}
	return len(done), false, s.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := state.DeleteJournal(ctx, tx, done); err != nil {
			return err
		}
		if s.OnView != nil && len(done) > 0 {
			return s.OnView(ctx, tx, k, done, resp.Run)
		}
		return nil
	})
}

// fence deletes what the Controller confirmed and marks the Run: from here its
// reports are kept, not sent.
func (s *Sender) fence(ctx context.Context, k state.Key, code domain.Code, confirmed []uint64, message string) error {
	s.Log.WarnContext(ctx, "the Controller refused a report for good: its reports are kept", "run", k.RunID, "lease_epoch", k.LeaseEpoch, "code", code, "message", message)
	return s.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := state.DeleteJournal(ctx, tx, confirmed); err != nil {
			return err
		}
		return state.Fence(ctx, tx, k, string(code))
	})
}
