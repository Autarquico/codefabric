package notify_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/client/notify"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

type fakeNative struct {
	mu    sync.Mutex
	shown [][2]string
}

func (f *fakeNative) Show(_ context.Context, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shown = append(f.shown, [2]string{title, body})
	return nil
}

func (f *fakeNative) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shown)
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func runFailed(seq uint64, run string) v1.Event {
	return v1.Event{Sequence: seq, Type: domain.EventRunFailed, RunID: run, ProjectID: "p1", Message: "run failed"}
}

func newNotifier(prefs config.ClientNotify, n notify.Native, clk *clock) *notify.Notifier {
	return notify.New(notify.Options{Prefs: prefs, Native: n, Now: clk.Now})
}

// fakeSource replays scripted streams: each FollowEvents call takes the next one.
type fakeSource struct {
	mu      sync.Mutex
	streams [][]v1.Event
	errs    []error
	afters  []string
}

func (s *fakeSource) followCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.afters)
}

func (s *fakeSource) Events(context.Context, client.EventQuery) (v1.EventPage, error) {
	return v1.EventPage{Cursor: "100"}, nil
}

func (s *fakeSource) FollowEvents(ctx context.Context, q client.EventQuery, fn func(v1.Event) error) error {
	s.mu.Lock()
	s.afters = append(s.afters, q.After)
	i := len(s.afters) - 1
	s.mu.Unlock()
	for _, e := range s.streams[i] {
		if err := fn(e); err != nil {
			return err
		}
	}
	if i < len(s.errs) {
		return s.errs[i]
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestScenario_PaletteSearchNotify_Dedupe(t *testing.T) {
	native := &fakeNative{}
	clk := &clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	n := newNotifier(config.ClientNotify{}, native, clk)

	expired := domain.NewError(domain.CodeCursorExpired, "purged", nil)
	// The same RunFailed arrives, repeats with a new sequence, and is replayed after a resync.
	src := &fakeSource{
		streams: [][]v1.Event{{runFailed(7, "r1"), runFailed(7, "r1"), runFailed(8, "r1")}, {runFailed(7, "r1")}},
		errs:    []error{expired},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for native.count() < 1 || src.followCalls() < 2 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := n.Watch(ctx, src); err != nil {
		t.Fatal(err)
	}
	if got := native.count(); got != 1 {
		t.Fatalf("the same RunFailed notified %d times, want 1", got)
	}
	if want := []string{"", "100"}; !slices.Equal(src.afters, want) {
		t.Errorf("resumed from %q, want %q", src.afters, want)
	}

	// A different Run is a different event; the same one after the window notifies again.
	n.Handle(ctx, runFailed(9, "r2"))
	clk.now = clk.now.Add(notify.DedupeWindow + time.Second)
	n.Handle(ctx, runFailed(10, "r1"))
	if got := native.count(); got != 3 {
		t.Errorf("after another Run and the window: %d notifications, want 3", got)
	}
}

func TestPrefsFilterEvents(t *testing.T) {
	native := &fakeNative{}
	clk := &clock{now: time.Now()}
	n := newNotifier(config.ClientNotify{MutedProjects: []string{"muted"}}, native, clk)
	ctx := context.Background()
	n.Handle(ctx, v1.Event{Sequence: 1, Type: domain.EventRunCompleted, RunID: "a"})                      // not a default type
	n.Handle(ctx, v1.Event{Sequence: 2, Type: domain.EventWorkerLost, WorkerID: "w", ProjectID: "muted"}) // muted Project
	n.Handle(ctx, v1.Event{Sequence: 3, Type: domain.EventWorkerLost, WorkerID: "w"})
	n.Handle(ctx, v1.Event{Sequence: 4, Type: domain.EventTaskNeedsInput, TaskID: "t"})
	if got := native.count(); got != 2 {
		t.Fatalf("%d notifications, want 2: %v", got, native.shown)
	}
	if got := n.Types(); !slices.Equal(got, []string{"RunFailed", "TaskNeedsInput", "WorkerLost"}) {
		t.Errorf("default types %v", got)
	}
}

func TestMutedEventDoesNotConsumeDedupe(t *testing.T) {
	native := &fakeNative{}
	n := newNotifier(config.ClientNotify{}, native, &clock{now: time.Now()})
	n.Handle(context.Background(), v1.Event{Sequence: 1, Type: domain.EventRunCompleted, RunID: "a"})
	n.Handle(context.Background(), runFailed(1, "a")) // same sequence, other type: the first one never notified
	if native.count() != 1 {
		t.Fatalf("got %d", native.count())
	}
}

func TestDedupeStateIsBounded(t *testing.T) {
	native := &fakeNative{}
	n := newNotifier(config.ClientNotify{}, native, &clock{now: time.Now()})
	ctx := context.Background()
	for i := range 10_000 {
		n.Handle(ctx, runFailed(uint64(i+1), "run-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String()))
	}
	// The oldest sequence has been forgotten: its replay notifies again (the state did not grow without bound).
	before := native.count()
	n.Handle(ctx, runFailed(1, "run-"+strings.Repeat("x", 0)+"a"+time.Duration(0).String()))
	if native.count() != before+1 {
		t.Errorf("the oldest key is still remembered after 10000 events")
	}
}

func TestQuietHoursHoldBackNativeOnly(t *testing.T) {
	native := &fakeNative{}
	var hooked int
	exec := func(context.Context, string, []string, []byte) ([]byte, error) { hooked++; return nil, nil }
	at := time.Date(2026, 1, 1, 23, 30, 0, 0, time.Local)
	clk := &clock{now: at}
	prefs := config.ClientNotify{QuietHours: &config.QuietHours{Start: "22:00", End: "07:00"}, Hook: []string{"hook"}}
	n := notify.New(notify.Options{Prefs: prefs, Native: native, Exec: exec, Now: clk.Now})
	n.Handle(context.Background(), runFailed(1, "a"))
	if native.count() != 0 || hooked != 1 {
		t.Fatalf("quiet: native %d hook %d", native.count(), hooked)
	}
	clk.now = time.Date(2026, 1, 2, 7, 0, 0, 0, time.Local)
	n.Handle(context.Background(), runFailed(2, "b"))
	if native.count() != 1 {
		t.Fatalf("after the quiet hours: native %d", native.count())
	}
}

func TestHookGetsEventJSONOnStdinAndATimeout(t *testing.T) {
	var gotName string
	var gotArgs []string
	var gotStdin []byte
	var deadline time.Time
	exec := func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
		gotName, gotArgs, gotStdin = name, args, stdin
		deadline, _ = ctx.Deadline()
		return []byte("ok"), nil
	}
	n := notify.New(notify.Options{Prefs: config.ClientNotify{Hook: []string{"/opt/hook", "--flag", "a b"}}, Exec: exec})
	n.Handle(context.Background(), runFailed(5, "r1"))
	if gotName != "/opt/hook" || !slices.Equal(gotArgs, []string{"--flag", "a b"}) {
		t.Errorf("argv %q %q", gotName, gotArgs)
	}
	if !strings.Contains(string(gotStdin), `"type":"RunFailed"`) || !strings.Contains(string(gotStdin), `"run_id":"r1"`) {
		t.Errorf("stdin %s", gotStdin)
	}
	if deadline.IsZero() {
		t.Error("the hook has no timeout")
	}
}

func TestHookFailureDoesNotStopNative(t *testing.T) {
	native := &fakeNative{}
	exec := func(context.Context, string, []string, []byte) ([]byte, error) { return nil, errors.New("boom") }
	n := notify.New(notify.Options{Prefs: config.ClientNotify{Hook: []string{"h"}}, Native: native, Exec: exec})
	n.Handle(context.Background(), runFailed(1, "a"))
	if native.count() != 1 {
		t.Fatal("native not shown")
	}
}

func TestNotificationTextIsSanitized(t *testing.T) {
	native := &fakeNative{}
	n := newNotifier(config.ClientNotify{}, native, &clock{now: time.Now()})
	e := runFailed(1, "a")
	e.Message = "\x1b[31mred\x1b[0m \x1b]0;title\x07text\x00\n" + strings.Repeat("z", 500)
	n.Handle(context.Background(), e)
	body := native.shown[0][1]
	if strings.ContainsAny(body, "\x1b\x00\n\x07") || !strings.HasPrefix(body, "red text") || len([]rune(body)) > 200 {
		t.Errorf("body %q", body)
	}
}

func TestNoNativeIsLoggedOnce(t *testing.T) {
	var log strings.Builder
	n := notify.New(notify.Options{Log: newLogger(&log)})
	n.Handle(context.Background(), runFailed(1, "a"))
	n.Handle(context.Background(), runFailed(2, "b"))
	if got := strings.Count(log.String(), "no native notifications"); got != 1 {
		t.Errorf("logged %d times:\n%s", got, log.String())
	}
}

func TestTestBypassesPrefsAndDedupe(t *testing.T) {
	native := &fakeNative{}
	n := newNotifier(config.ClientNotify{Events: []string{}}, native, &clock{now: time.Now()})
	n.Test(context.Background())
	n.Test(context.Background())
	if native.count() != 2 {
		t.Fatalf("got %d", native.count())
	}
}
