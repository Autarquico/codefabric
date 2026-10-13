// Package events is the use cases that read the event store (§20, §31.4): a
// page of events, and the follow that keeps delivering them. The store is the
// only source of truth; the Notifier only says when to look again.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/stream"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const maxTypeLen = 64

// Service runs the event use cases.
type Service struct {
	db    *store.DB
	clock clock.Clock
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock) *Service { return &Service{db: db, clock: clk} }

// Query is GET /v1/events. Project, Task, Run and Worker are references (§3.13).
// Tail asks for the last events instead of a page after a cursor.
type Query struct {
	After   uint64
	Tail    int
	Limit   int
	Project string
	Task    string
	Run     string
	Worker  string
	Types   []domain.EventType
	Level   string
	Since   time.Time
}

// ParseCursor reads a cursor; the empty one is the start of the history.
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
	case q.Tail < 0 || q.Tail > v1.EventsMaxTail:
		return input.Invalid("tail must be between 0 and %d", v1.EventsMaxTail)
	case q.Limit < 0 || q.Limit > v1.EventsMaxLimit:
		return input.Invalid("limit must be between 0 and %d", v1.EventsMaxLimit)
	case q.Tail > 0 && q.After > 0:
		return input.Invalid("tail and after exclude each other")
	case q.Tail > 0 && q.Limit > 0:
		return input.Invalid("tail and limit exclude each other")
	case len(q.Types) > v1.EventsMaxTypes:
		return input.Invalid("at most %d types", v1.EventsMaxTypes)
	case q.Level != "" && !domain.ValidLevel(q.Level):
		return input.Invalid("level must be debug, info, warn or error")
	}
	for _, t := range q.Types {
		if err := eventType(string(t)); err != nil {
			return err
		}
	}
	return nil
}

func eventType(t string) error {
	if t == "" || len(t) > maxTypeLen {
		return input.Invalid("a type is a name of at most %d letters", maxTypeLen)
	}
	for _, r := range t {
		if !unicode.IsLetter(r) || r > unicode.MaxASCII {
			return input.Invalid("a type is a name of at most %d letters", maxTypeLen)
		}
	}
	return nil
}

// resolve turns the references of the query into the filter of the store.
func (q Query) resolve(ctx context.Context, r store.Querier) (store.EventFilter, error) {
	f := store.EventFilter{Types: q.Types, MinLevel: q.Level, Since: q.Since}
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

// List answers GET /v1/events. The page, the high-water mark it is cut at and
// the bounds of the history are read in one snapshot. The cursor of the answer
// is the sequence of the last event when the page is full, whether by count or
// by bytes; otherwise it is the mark itself, so that a filtered follow does not
// read the same stretch of the table again at every wake.
func (s *Service) List(ctx context.Context, by pki.Session, q Query) (v1.EventPage, error) {
	if err := q.validate(); err != nil {
		return v1.EventPage{}, err
	}
	seen := s.db.Notifier().Latest()
	var page v1.EventPage
	err := s.db.Read(ctx, func(r store.Querier) error {
		filter, err := q.resolve(ctx, r)
		if err != nil {
			return err
		}
		if q.Tail > 0 {
			page, err = s.tail(ctx, r, by, filter, seen, q.Tail)
			return err
		}
		page, err = s.pageIn(ctx, r, by, filter, q.After, seen, cmpLimit(q.Limit))
		return err
	})
	return page, err
}

func cmpLimit(limit int) int {
	if limit <= 0 {
		return v1.EventsDefaultLimit
	}
	return limit
}

// tail is the last events at or below the mark that match, oldest first, within
// the bytes of a page. The first one is always sent.
func (s *Service) tail(ctx context.Context, r store.Querier, by pki.Session, f store.EventFilter, seen uint64, n int) (v1.EventPage, error) {
	if _, err := pki.RequireCaller(ctx, r, by, s.clock.Now()); err != nil {
		return v1.EventPage{}, err
	}
	stored, err := store.EventsTail(ctx, r, seen, n, f)
	if err != nil {
		return v1.EventPage{}, err
	}
	page := v1.EventPage{Events: []v1.Event{}, Cursor: formatCursor(seen)}
	used, from := 0, len(stored)
	for i := len(stored) - 1; i >= 0; i-- {
		size, err := encodedSize(view(stored[i]))
		if err != nil {
			return v1.EventPage{}, err
		}
		if i < len(stored)-1 && used+size > v1.EventsPageBytes {
			break
		}
		used, from = used+size, i
	}
	for _, e := range stored[from:] {
		page.Events = append(page.Events, view(e))
	}
	return page, nil
}

// pageIn is one look at the history inside the snapshot r. The caller is
// authorised again: a follow can outlive the identity that opened it.
func (s *Service) pageIn(ctx context.Context, r store.Querier, by pki.Session, f store.EventFilter, after, seen uint64, limit int) (v1.EventPage, error) {
	if _, err := pki.RequireCaller(ctx, r, by, s.clock.Now()); err != nil {
		return v1.EventPage{}, err
	}
	latest, purged, err := store.EventBounds(ctx, r)
	if err != nil {
		return v1.EventPage{}, err
	}
	// The cursor 0 is the start of whatever is held: only one that was issued can be lost.
	if (after != 0 && after < purged) || after > latest {
		return v1.EventPage{}, domain.NewError(domain.CodeCursorExpired,
			"the cursor is outside the history this Controller holds",
			map[string]any{"purged_through": purged, "latest": latest})
	}
	stored, err := store.EventsAfter(ctx, r, after, seen, limit, f)
	if err != nil {
		return v1.EventPage{}, err
	}
	page := v1.EventPage{Events: []v1.Event{}, Cursor: formatCursor(after)}
	full := len(stored) == limit
	used := 0
	for i, e := range stored {
		ev := view(e)
		size, err := encodedSize(ev)
		if err != nil {
			return v1.EventPage{}, err
		}
		if i > 0 && used+size > v1.EventsPageBytes {
			full = true
			break
		}
		used += size
		page.Events = append(page.Events, ev)
		page.Cursor = formatCursor(e.Sequence)
	}
	page.HasMore = full
	if !full {
		page.Cursor = formatCursor(max(after, seen, lastSequence(page.Events)))
	}
	return page, nil
}

func lastSequence(events []v1.Event) uint64 {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Sequence
}

func encodedSize(e v1.Event) (int, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("events: encoding event %d: %w", e.Sequence, err)
	}
	return len(raw), nil
}

func view(e domain.Event) v1.Event {
	return v1.Event{
		Sequence: e.Sequence, ID: string(e.ID), SchemaVersion: e.SchemaVersion, Timestamp: e.Timestamp, ReceivedAt: e.ReceivedAt,
		Source: e.Source, WorkerEpoch: e.WorkerEpoch, WorkerSeq: e.WorkerSeq, RequestID: e.RequestID,
		Type: e.Type, Level: e.Level, Actor: v1.EventActor{Kind: e.Actor.Kind, ID: e.Actor.ID},
		ProjectID: string(e.ProjectID), PlanID: string(e.PlanID), TaskID: string(e.TaskID), RunID: string(e.RunID),
		WorkerID: string(e.WorkerID), WorkflowID: string(e.WorkflowID), Message: e.Message, Data: e.Data,
	}
}

// Follower keeps delivering the events after a cursor to one client.
type Follower struct {
	s      *Service
	by     pki.Session
	filter store.EventFilter
	after  uint64
}

// Open validates a follow and resolves its references, before anything is
// sent: a bad filter, an expired cursor or a revoked caller fail here, where
// the transport can still answer with a plain error. after is the cursor to
// resume from; Latest is "from now".
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
		_, err = s.pageIn(ctx, r, by, f.filter, after, s.db.Notifier().Latest(), 1)
		return err
	})
	return f, err
}

// Latest is the sequence of the newest event announced: a follow that starts
// there delivers only what happens from now on.
func (s *Service) Latest() uint64 { return s.db.Notifier().Latest() }

// Run delivers pages until ctx ends or deliver or idle fail, or the caller or
// the cursor stops being valid. A page is delivered with the cursor that
// resumes after it. idle is called when the stream has been silent for
// stream.KeepaliveEvery, after the caller was authorised again.
//
// It looks, and only if there is nothing more waits, from the mark it read
// before looking: a commit between the two is never missed. A commit that is
// not for this stream only makes it look again, from where the last look ended.
func (f *Follower) Run(ctx context.Context, deliver func(events []v1.Event, cursor string) error, idle func() error) error {
	pacer := stream.NewPacer(f.s.clock, stream.KeepaliveEvery)
	for {
		seen := f.s.db.Notifier().Latest()
		var page v1.EventPage
		err := f.s.db.Read(ctx, func(r store.Querier) (err error) {
			page, err = f.s.pageIn(ctx, r, f.by, f.filter, f.after, seen, v1.EventsDefaultLimit)
			return err
		})
		if err != nil {
			return err
		}
		f.after, _ = ParseCursor(page.Cursor)
		if len(page.Events) > 0 {
			if err := deliver(page.Events, page.Cursor); err != nil {
				return err
			}
			pacer.Wrote()
		}
		if page.HasMore {
			continue
		}
		for {
			due, err := pacer.Wait(ctx, f.s.db.Notifier().Changed(seen), 0)
			if err != nil {
				return err
			}
			if !due {
				break
			}
			if err := f.s.authorize(ctx, f.by); err != nil {
				return err
			}
			if err := idle(); err != nil {
				return err
			}
			pacer.Wrote()
			if f.s.db.Notifier().Latest() > seen {
				break
			}
		}
	}
}

func (s *Service) authorize(ctx context.Context, by pki.Session) error {
	return s.db.Read(ctx, func(r store.Querier) error {
		_, err := pki.RequireCaller(ctx, r, by, s.clock.Now())
		return err
	})
}
