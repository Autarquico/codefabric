// Package logs is the use cases of the structured logs (§3.12, §20.3, §34):
// the lines the Workers ship and those of the Controller itself go into one
// table, ordered by its sequence; clients read a page of it or follow it.
// The store is the only source of truth; the notifier only says when to look
// again, and is told only after a commit.
package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/stream"
	"github.com/Autarquico/codefabric/internal/controller/eventbus"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
)

// Service runs the log use cases. Every write of the logs goes through it, so
// its notifier hears of every commit.
type Service struct {
	db       *store.DB
	clock    clock.Clock
	notifier *eventbus.Notifier
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock) *Service {
	return &Service{db: db, clock: clk, notifier: eventbus.NewNotifier()}
}

// Ingest is POST /v1/worker/logs: lines of the Worker the caller stands for.
// A line already stored is a duplicate; a malformed one is refused for good
// and stores nothing; neither stops the others. A Run the Worker never held is
// not named: the line is kept without it. The Task and the Project are those of
// the Run, never what the Worker says.
func (s *Service) Ingest(ctx context.Context, by pki.Session, b v1.LogBatch) (v1.LogBatchResponse, error) {
	if len(b.Entries) > v1.LogBatchMaxEntries {
		return v1.LogBatchResponse{}, input.Invalid("at most %d entries in a batch", v1.LogBatchMaxEntries)
	}
	size := 0
	for _, e := range b.Entries {
		size += len(e.Message) + len(e.Attrs)
	}
	if size > v1.LogBatchBytes {
		return v1.LogBatchResponse{}, input.Invalid("the entries of a batch hold at most %d bytes", v1.LogBatchBytes)
	}
	var (
		out  v1.LogBatchResponse
		last uint64
	)
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		out, last = v1.LogBatchResponse{}, 0
		w, err := workerOf(ctx, tx, by, s.clock.Now())
		if err != nil {
			return err
		}
		held := map[string]heldRun{}
		for _, e := range b.Entries {
			if !wellFormed(e) {
				out.Rejected++
				continue
			}
			rec := store.LogRecord{
				Timestamp: e.Timestamp, WorkerID: w.ID, WorkerEpoch: e.WorkerEpoch, WorkerSeq: &e.WorkerSeq,
				Level: e.Level, Message: e.Message, Attrs: e.Attrs,
			}
			if e.RunID != "" {
				h, ok := held[e.RunID]
				if !ok {
					if h.task, h.project, h.ok, err = store.RunHeldBy(ctx, tx, domain.ID(e.RunID), w.ID); err != nil {
						return err
					}
					held[e.RunID] = h
				}
				if h.ok {
					rec.RunID, rec.TaskID, rec.ProjectID = domain.ID(e.RunID), h.task, h.project
				}
			}
			seq, dup, err := tx.AppendLog(ctx, rec)
			if err != nil {
				return err
			}
			if dup {
				out.Duplicate++
				continue
			}
			out.Applied, last = out.Applied+1, seq
		}
		return nil
	})
	if err != nil {
		return v1.LogBatchResponse{}, err
	}
	s.notifier.Notify(last)
	return out, nil
}

type heldRun struct {
	task, project domain.ID
	ok            bool
}

func workerOf(ctx context.Context, q store.Querier, by pki.Session, now time.Time) (store.Worker, error) {
	if _, err := pki.RequireCaller(ctx, q, by, now); err != nil {
		return store.Worker{}, err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Worker{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	return w, err
}

// wellFormed says whether e is within the bounds of a line. The Worker cuts
// its lines to them, so one that is not is counted as rejected and not stored.
func wellFormed(e v1.LogEntry) bool {
	return e.WorkerEpoch != "" && len(e.WorkerEpoch) <= v1.LogEpochMaxLen && utf8.ValidString(e.WorkerEpoch) &&
		e.WorkerSeq != 0 && e.WorkerSeq <= math.MaxInt64 &&
		!e.Timestamp.IsZero() &&
		domain.ValidLevel(e.Level) &&
		len(e.Message) <= v1.LogMessageMaxBytes && utf8.ValidString(e.Message) &&
		len(e.Attrs) <= v1.LogAttrsMaxBytes && (len(e.Attrs) == 0 || jsonObject(e.Attrs)) &&
		len(e.RunID) <= v1.LogRunIDMaxLen
}

func jsonObject(raw []byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}

// AppendController stores lines of the Controller's own log: the sink of its
// slogtee. They are trusted as they are, redacted by the tee.
func (s *Service) AppendController(ctx context.Context, entries []slogtee.Entry) error {
	var last uint64
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		for _, e := range entries {
			seq, _, err := tx.AppendLog(ctx, store.LogRecord{
				Timestamp: e.Time, Level: e.Level, Message: e.Msg, Attrs: e.Attrs,
				RunID: domain.ID(e.RunID), TaskID: domain.ID(e.TaskID), ProjectID: domain.ID(e.ProjectID),
			})
			if err != nil {
				return err
			}
			last = seq
		}
		return nil
	})
	if err == nil {
		s.notifier.Notify(last)
	}
	return err
}

// Query is GET /v1/logs. Project, Task, Run and Worker are references (§3.13);
// Level is the lowest level kept; Since is on the Controller's clock. Tail
// asks for the last lines instead of a page after a cursor.
type Query struct {
	After   uint64
	Tail    int
	Limit   int
	Project string
	Task    string
	Run     string
	Worker  string
	Level   string
	Since   time.Time
}

// ParseCursor reads a cursor; the empty one is the start of the logs.
func ParseCursor(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return 0, input.Invalid("the cursor is not one this Controller issued")
	}
	return n, nil
}

func formatCursor(seq uint64) string { return strconv.FormatUint(seq, 10) }

func (q Query) validate() error {
	switch {
	case q.Tail < 0 || q.Tail > v1.LogsMaxTail:
		return input.Invalid("tail must be between 0 and %d", v1.LogsMaxTail)
	case q.Limit < 0 || q.Limit > v1.LogsMaxLimit:
		return input.Invalid("limit must be between 0 and %d", v1.LogsMaxLimit)
	case q.Tail > 0 && (q.After > 0 || q.Limit > 0):
		return input.Invalid("tail excludes after and limit")
	case q.Level != "" && !domain.ValidLevel(q.Level):
		return input.Invalid("level must be debug, info, warn or error")
	}
	return nil
}

func (q Query) resolve(ctx context.Context, r store.Querier) (store.LogFilter, error) {
	f := store.LogFilter{MinLevel: q.Level, Since: q.Since}
	scope, err := refs.Scope(ctx, r, q.Project)
	if err != nil {
		return f, err
	}
	f.ProjectID = scope
	if q.Task != "" {
		if f.TaskID, err = refs.Task(ctx, r, q.Task, scope); err != nil {
			return f, err
		}
	}
	if q.Run != "" {
		if f.RunID, err = refs.Run(ctx, r, q.Run); err != nil {
			return f, err
		}
	}
	if q.Worker != "" {
		cands, err := store.WorkerCandidates(ctx, r)
		if err != nil {
			return f, err
		}
		if f.WorkerID, err = domain.Resolve(q.Worker, domain.KindWorker, "", cands); err != nil {
			return f, err
		}
	}
	return f, nil
}

// List answers GET /v1/logs, in one snapshot cut at the highest sequence it
// holds. The cursor is the sequence of the last line when the page is full;
// otherwise the cut, so that a filtered follow does not read the same stretch
// again at every wake.
func (s *Service) List(ctx context.Context, by pki.Session, q Query) (v1.LogPage, error) {
	if err := q.validate(); err != nil {
		return v1.LogPage{}, err
	}
	var page v1.LogPage
	err := s.db.Read(ctx, func(r store.Querier) error {
		f, err := q.resolve(ctx, r)
		if err != nil {
			return err
		}
		if q.Tail > 0 {
			page, err = s.tail(ctx, r, by, f, q.Tail)
			return err
		}
		page, err = s.pageIn(ctx, r, by, f, q.After, pageLimit(q.Limit))
		return err
	})
	return page, err
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return v1.LogsDefaultLimit
	}
	return limit
}

func (s *Service) tail(ctx context.Context, r store.Querier, by pki.Session, f store.LogFilter, n int) (v1.LogPage, error) {
	if _, err := pki.RequireCaller(ctx, r, by, s.clock.Now()); err != nil {
		return v1.LogPage{}, err
	}
	mark, err := store.LatestLogSequence(ctx, r)
	if err != nil {
		return v1.LogPage{}, err
	}
	stored, err := store.LogsTail(ctx, r, mark, n, f)
	if err != nil {
		return v1.LogPage{}, err
	}
	used, from := 0, len(stored)
	for i := len(stored) - 1; i >= 0; i-- {
		size, err := encodedSize(view(stored[i]))
		if err != nil {
			return v1.LogPage{}, err
		}
		if i < len(stored)-1 && used+size > v1.LogsPageBytes {
			break
		}
		used, from = used+size, i
	}
	page := v1.LogPage{Logs: make([]v1.Log, 0, len(stored)-from), Cursor: formatCursor(mark)}
	for _, l := range stored[from:] {
		page.Logs = append(page.Logs, view(l))
	}
	return page, nil
}

// pageIn is one look at the logs inside the snapshot r. The caller is
// authorised again: a follow can outlive the identity that opened it.
func (s *Service) pageIn(ctx context.Context, r store.Querier, by pki.Session, f store.LogFilter, after uint64, limit int) (v1.LogPage, error) {
	if _, err := pki.RequireCaller(ctx, r, by, s.clock.Now()); err != nil {
		return v1.LogPage{}, err
	}
	mark, err := store.LatestLogSequence(ctx, r)
	if err != nil {
		return v1.LogPage{}, err
	}
	if after > mark {
		return v1.LogPage{}, domain.NewError(domain.CodeCursorExpired,
			"the cursor is outside the logs this Controller holds", map[string]any{"latest": mark})
	}
	stored, err := store.LogsAfter(ctx, r, after, mark, limit, f)
	if err != nil {
		return v1.LogPage{}, err
	}
	page := v1.LogPage{Logs: []v1.Log{}, Cursor: formatCursor(after)}
	full, used := len(stored) == limit, 0
	for i, l := range stored {
		lv := view(l)
		size, err := encodedSize(lv)
		if err != nil {
			return v1.LogPage{}, err
		}
		if i > 0 && used+size > v1.LogsPageBytes {
			full = true
			break
		}
		used += size
		page.Logs = append(page.Logs, lv)
		page.Cursor = formatCursor(l.Sequence)
	}
	page.HasMore = full
	if !full {
		page.Cursor = formatCursor(mark)
	}
	return page, nil
}

func encodedSize(l v1.Log) (int, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return 0, fmt.Errorf("logs: encoding log %d: %w", l.Sequence, err)
	}
	return len(raw), nil
}

func view(l store.LogRecord) v1.Log {
	return v1.Log{
		Sequence: l.Sequence, Timestamp: l.Timestamp, ReceivedAt: l.ReceivedAt, Source: l.Source,
		WorkerID: string(l.WorkerID), WorkerEpoch: l.WorkerEpoch, WorkerSeq: l.WorkerSeq,
		Level: l.Level, Message: l.Message, Attrs: json.RawMessage(l.Attrs),
		ProjectID: string(l.ProjectID), TaskID: string(l.TaskID), RunID: string(l.RunID),
	}
}

// Latest is the highest sequence stored: a follow that starts there delivers
// only what is logged from now on.
func (s *Service) Latest(ctx context.Context) (uint64, error) {
	var seq uint64
	err := s.db.Read(ctx, func(r store.Querier) (err error) {
		seq, err = store.LatestLogSequence(ctx, r)
		return err
	})
	return seq, err
}

// Follower keeps delivering the lines after a cursor to one client.
type Follower struct {
	s      *Service
	by     pki.Session
	filter store.LogFilter
	after  uint64
}

// Open validates a follow and resolves its references before anything is
// sent: a bad filter, a cursor ahead of the logs or a revoked caller fail here.
func (s *Service) Open(ctx context.Context, by pki.Session, q Query, after uint64) (*Follower, error) {
	if q.Tail != 0 {
		return nil, input.Invalid("a stream has no tail: read the tail, then follow from its cursor")
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	f := &Follower{s: s, by: by, after: after}
	err := s.db.Read(ctx, func(r store.Querier) error {
		var err error
		if f.filter, err = q.resolve(ctx, r); err != nil {
			return err
		}
		_, err = s.pageIn(ctx, r, by, f.filter, after, 1)
		return err
	})
	return f, err
}

// Run delivers pages until ctx ends, deliver or idle fail, or the caller stops
// being valid. It reads the mark of the notifier before it looks, and waits
// only past that mark: a commit after the look is announced above it, so none
// is missed. idle is called when the stream has been quiet for
// stream.KeepaliveEvery, after the caller was authorised again.
func (f *Follower) Run(ctx context.Context, deliver func(logs []v1.Log, cursor string) error, idle func() error) error {
	return stream.Follow(ctx, f.s.clock, f.s.notifier,
		func(uint64) (bool, bool, error) {
			var page v1.LogPage
			err := f.s.db.Read(ctx, func(r store.Querier) (err error) {
				page, err = f.s.pageIn(ctx, r, f.by, f.filter, f.after, v1.LogsDefaultLimit)
				return err
			})
			if err != nil {
				return false, false, err
			}
			f.after, _ = ParseCursor(page.Cursor)
			if len(page.Logs) == 0 {
				return false, page.HasMore, nil
			}
			return true, page.HasMore, deliver(page.Logs, page.Cursor)
		},
		func() error { return f.s.authorize(ctx, f.by) }, idle)
}

func (s *Service) authorize(ctx context.Context, by pki.Session) error {
	return s.db.Read(ctx, func(r store.Querier) error {
		_, err := pki.RequireCaller(ctx, r, by, s.clock.Now())
		return err
	})
}
