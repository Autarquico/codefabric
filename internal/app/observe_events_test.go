package app_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/events"
	"github.com/Autarquico/codefabric/internal/app/stream"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// emit appends controller events, announced as any committed event is.
func (e *env) emit(n int, mutate func(i int, ev *domain.Event)) {
	e.t.Helper()
	for i := range n {
		ev := domain.Event{Type: domain.EventTaskCreated, Level: "info", Message: "m" + strconv.Itoa(i), Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "test"}}
		if mutate != nil {
			mutate(i, &ev)
		}
		must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
			_, _, err := tx.AppendEvent(e.t.Context(), ev)
			return err
		}))
	}
}

func (e *env) list(q events.Query) v1.EventPage {
	e.t.Helper()
	page, err := e.app.Events.List(e.t.Context(), e.owner, q)
	must(e.t, err)
	return page
}

func seqs(events []v1.Event) []uint64 {
	var out []uint64
	for _, ev := range events {
		out = append(out, ev.Sequence)
	}
	return out
}

func TestEvents_ACommittedEventNotYetAnnouncedIsDeliveredWithTheNextAnnouncement(t *testing.T) {
	e := newEnv(t)
	e.emit(1, nil)
	// Committed, but the process has not announced it: the gap between a commit and its Notify.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO events (id, schema_version, ts, received_at, source, type, level, actor_kind, actor_id, message)
			 VALUES ('event_unannounced', 1, 1, 1, 'controller', 'TaskCreated', 'info', 'system', 'test', 'late')`)
		return err
	}))

	page := e.list(events.Query{})
	if !slices.Equal(seqs(page.Events), []uint64{1}) || page.Cursor != "1" || page.HasMore {
		t.Fatalf("before the announcement: %+v", page)
	}
	// Reading on from that cursor loses nothing once the commit is announced.
	e.db.Notifier().Notify(2)
	page = e.list(events.Query{After: 1})
	if !slices.Equal(seqs(page.Events), []uint64{2}) || page.Cursor != "2" {
		t.Fatalf("after the announcement: %+v", page)
	}
}

func TestEvents_TheCursorOfAFilteredPageSkipsWhatDoesNotMatch(t *testing.T) {
	e := newEnv(t)
	e.emit(6, func(i int, ev *domain.Event) {
		if i == 2 {
			ev.Type = domain.EventRunFailed
		}
	})
	page := e.list(events.Query{Types: []domain.EventType{domain.EventRunFailed}})
	if !slices.Equal(seqs(page.Events), []uint64{3}) || page.Cursor != "6" || page.HasMore {
		t.Fatalf("page = %+v", page)
	}
	// Nothing matches: the cursor still moves to the mark, so the table is not read again from the start.
	page = e.list(events.Query{Types: []domain.EventType{domain.EventRunCancelled}, After: 1})
	if len(page.Events) != 0 || page.Cursor != "6" {
		t.Fatalf("page = %+v", page)
	}
}

func TestEvents_ACountFullPageKeepsTheCursorAtItsLastEvent(t *testing.T) {
	e := newEnv(t)
	e.emit(5, nil)
	page := e.list(events.Query{Limit: 2})
	if !slices.Equal(seqs(page.Events), []uint64{1, 2}) || page.Cursor != "2" || !page.HasMore {
		t.Fatalf("page = %+v", page)
	}
	page = e.list(events.Query{Limit: 3, After: 2})
	if !slices.Equal(seqs(page.Events), []uint64{3, 4, 5}) || page.Cursor != "5" || !page.HasMore {
		t.Fatalf("a page that is exactly full is full: %+v", page)
	}
	if page = e.list(events.Query{Limit: 3, After: 5}); len(page.Events) != 0 || page.HasMore || page.Cursor != "5" {
		t.Fatalf("page = %+v", page)
	}
}

func TestEvents_APageCutByBytesIsFullAndSkipsNothing(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("x", 20<<10)
	e.emit(40, func(_ int, ev *domain.Event) { ev.Message = big })

	var got []uint64
	after, pages := uint64(0), 0
	for {
		page := e.list(events.Query{After: after, Limit: v1.EventsMaxLimit})
		pages++
		got = append(got, seqs(page.Events)...)
		cursor, err := events.ParseCursor(page.Cursor)
		must(t, err)
		if len(page.Events) == 0 {
			t.Fatal("an empty page with more to read")
		}
		if cursor != page.Events[len(page.Events)-1].Sequence && page.HasMore {
			t.Fatalf("a full page's cursor %d is not its last event", cursor)
		}
		after = cursor
		if !page.HasMore {
			break
		}
	}
	if pages < 2 || len(got) != 40 {
		t.Fatalf("%d pages, %d events", pages, len(got))
	}
	for i, s := range got {
		if s != uint64(i)+1 { //nolint:gosec // small index
			t.Fatalf("events %v are not contiguous", got)
		}
	}
	// The first event of a page is always sent, however big.
	if page := e.list(events.Query{Limit: 1}); len(page.Events) != 1 {
		t.Fatalf("page = %d events", len(page.Events))
	}
}

func TestEvents_TheCursorIsCheckedAgainstWhatTheHistoryHolds(t *testing.T) {
	e := newEnv(t)
	e.emit(5, nil)
	_, err := e.app.Events.List(t.Context(), e.owner, events.Query{After: 6})
	wantCode(t, err, domain.CodeCursorExpired)
	if page := e.list(events.Query{After: 5}); len(page.Events) != 0 {
		t.Fatalf("the latest cursor: %+v", page)
	}

	// The retention of Phase 4 will write this key; a cursor below it is lost history.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO controller_meta (key, value) VALUES ('events_purged_through', '3')`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `DELETE FROM events WHERE sequence <= 3`)
		return err
	}))
	_, err = e.app.Events.List(t.Context(), e.owner, events.Query{After: 2})
	wantCode(t, err, domain.CodeCursorExpired)
	if page := e.list(events.Query{After: 3}); !slices.Equal(seqs(page.Events), []uint64{4, 5}) {
		t.Fatalf("after == purged_through: %+v", page)
	}
	// The empty cursor is the start of whatever is still held, not a lost position.
	if page := e.list(events.Query{}); !slices.Equal(seqs(page.Events), []uint64{4, 5}) {
		t.Fatalf("from the start with purged events: %+v", page)
	}
}

func TestEvents_ListChecksTheCallerAndTheQuery(t *testing.T) {
	e := newEnv(t)
	e.emit(1, nil)
	for name, q := range map[string]events.Query{
		"tail with after": {Tail: 1, After: 1},
		"tail with limit": {Tail: 1, Limit: 1},
		"big tail":        {Tail: v1.EventsMaxTail + 1},
		"big limit":       {Limit: v1.EventsMaxLimit + 1},
		"bad level":       {Level: "loud"},
		"bad type":        {Types: []domain.EventType{"Run Queued"}},
		"too many types":  {Types: make([]domain.EventType, v1.EventsMaxTypes+1)},
	} {
		if _, err := e.app.Events.List(t.Context(), e.owner, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	revoked := e.newClient("gone")
	must(t, e.authority.RevokeClient(t.Context(), e.owner, domain.ID(revoked.Principal.ID)))
	_, err := e.app.Events.List(t.Context(), revoked, events.Query{})
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestEvents_TailKeepsTheNewestWithinTheBytesOfAPage(t *testing.T) {
	e := newEnv(t)
	e.emit(40, func(_ int, ev *domain.Event) { ev.Message = strings.Repeat("y", 20<<10) })
	page := e.list(events.Query{Tail: 40})
	if len(page.Events) >= 40 || len(page.Events) == 0 || page.Events[len(page.Events)-1].Sequence != 40 || page.Cursor != "40" {
		t.Fatalf("tail = %d events, cursor %s", len(page.Events), page.Cursor)
	}
	for i := 1; i < len(page.Events); i++ {
		if page.Events[i].Sequence != page.Events[i-1].Sequence+1 {
			t.Fatalf("tail is not contiguous: %v", seqs(page.Events))
		}
	}
}

// follow runs a Follower until the test ends and collects what it delivers.
type followed struct {
	mu      sync.Mutex
	got     []uint64
	idles   int
	err     error
	stopped chan struct{}
	cancel  context.CancelFunc
}

func (f *followed) delivered() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *followed) idled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idles
}

func (e *env) follow(q events.Query, after uint64) *followed {
	e.t.Helper()
	fl, err := e.app.Events.Open(e.t.Context(), e.owner, q, after)
	must(e.t, err)
	ctx, cancel := context.WithCancel(e.t.Context())
	f := &followed{stopped: make(chan struct{}), cancel: cancel}
	e.t.Cleanup(cancel)
	go func() {
		defer close(f.stopped)
		err := fl.Run(ctx,
			func(batch []v1.Event, _ string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.got = append(f.got, seqs(batch)...)
				return nil
			},
			func() error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.idles++
				return nil
			})
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
	}()
	return f
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestEventFollow_DeliversInOrderFromTheCursorAndKeepsAlive(t *testing.T) {
	e := newEnv(t)
	e.emit(3, nil)
	f := e.follow(events.Query{}, 1)
	waitUntil(t, "the backlog", func() bool { return slices.Equal(f.delivered(), []uint64{2, 3}) })

	e.emit(2, nil)
	waitUntil(t, "live events", func() bool { return slices.Equal(f.delivered(), []uint64{2, 3, 4, 5}) })

	must(t, e.clock.BlockUntil(t.Context(), 1))
	e.clock.Advance(stream.KeepaliveEvery)
	waitUntil(t, "the keepalive", func() bool { return f.idled() == 1 })

	f.cancel()
	<-f.stopped
	if f.err == nil || f.err != context.Canceled { //nolint:errorlint // set by this test
		t.Fatalf("err = %v", f.err)
	}
}

func TestEventFollow_ARevokedCallerEndsItWithUnauthenticated(t *testing.T) {
	e := newEnv(t)
	laptop := e.newClient("laptop")
	fl, err := e.app.Events.Open(t.Context(), laptop, events.Query{}, 0)
	must(t, err)
	done := make(chan error, 1)
	go func() {
		done <- fl.Run(t.Context(), func([]v1.Event, string) error { return nil }, func() error { return nil })
	}()
	must(t, e.authority.RevokeClient(t.Context(), e.owner, domain.ID(laptop.Principal.ID)))
	e.emit(1, nil) // wakes the follow, which looks again
	select {
	case err := <-done:
		wantCode(t, err, domain.CodeUnauthenticated)
	case <-time.After(5 * time.Second):
		t.Fatal("the follow outlived its caller's identity")
	}
}

func TestEventFollow_OpenRefusesWhatCannotBeFollowed(t *testing.T) {
	e := newEnv(t)
	e.emit(1, nil)
	_, err := e.app.Events.Open(t.Context(), e.owner, events.Query{}, 99)
	wantCode(t, err, domain.CodeCursorExpired)
	_, err = e.app.Events.Open(t.Context(), e.owner, events.Query{Tail: 5}, 0)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = e.app.Events.Open(t.Context(), e.owner, events.Query{Run: "run-9"}, 0)
	wantCode(t, err, domain.CodeNotFound)
}
