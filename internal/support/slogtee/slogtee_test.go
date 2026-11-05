package slogtee_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
)

// quiet is an inner handler at info that writes nowhere.
var quiet = slog.NewTextHandler(io.Discard, nil)

// collector is a sink that keeps what it is given.
type collector struct {
	mu      sync.Mutex
	entries []slogtee.Entry
}

func (c *collector) sink(_ context.Context, es []slogtee.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, es...)
	return nil
}

func (c *collector) all() []slogtee.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slogtee.Entry(nil), c.entries...)
}

// run starts the writer and returns the function that stops it and waits.
func run(t *testing.T, tee *slogtee.Tee, sink slogtee.Sink, fallback *slog.Logger) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); tee.Run(ctx, sink, fallback) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the writer did not stop")
		}
	}
}

func attrs(t *testing.T, e slogtee.Entry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Attrs, &m); err != nil {
		t.Fatalf("attrs %s: %v", e.Attrs, err)
	}
	return m
}

type secretValue string

func (secretValue) LogValue() slog.Value { return slog.StringValue("[hidden]") }

func TestTheInnerHandlerGetsEveryRecordAndTheTeeAJSONCopyWithItsIDs(t *testing.T) {
	var stderr bytes.Buffer
	tee := slogtee.New(slogtee.Options{})
	log := slog.New(tee.Wrap(slog.NewTextHandler(&stderr, nil)))
	var c collector
	stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))

	log.With("run", "run_01J", "project", "project-1").WithGroup("g").Info("hello", "n", 3, "task", "task_01J", "v", secretValue("x"))
	log.Debug("not enabled") // the inner handler is at info: the tee follows it
	log.Warn("plain")
	stop()

	if !strings.Contains(stderr.String(), "msg=hello") || !strings.Contains(stderr.String(), "g.n=3") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	got := c.all()
	if len(got) != 2 {
		t.Fatalf("%d entries, want 2: %+v", len(got), got)
	}
	e := got[0]
	if e.Msg != "hello" || e.Level != "info" || e.Time.IsZero() {
		t.Fatalf("entry = %+v", e)
	}
	// run comes from With at the top level; task is inside a group, project is not an ID.
	if e.RunID != "run_01J" || e.TaskID != "" || e.ProjectID != "" {
		t.Fatalf("ids = %q %q %q", e.RunID, e.TaskID, e.ProjectID)
	}
	m := attrs(t, e)
	g, _ := m["g"].(map[string]any)
	if m["run"] != "run_01J" || g["n"] != float64(3) || g["v"] != "[hidden]" {
		t.Fatalf("attrs = %s", e.Attrs)
	}
	if _, ok := m["level"]; ok {
		t.Fatalf("the level is a column, not an attribute: %s", e.Attrs)
	}
	if got[1].Level != "warn" || string(got[1].Attrs) != "{}" {
		t.Fatalf("second = %+v", got[1])
	}
}

func TestTheCopyIsRedacted(t *testing.T) {
	const secret = "s3cr3t-value-123456"
	tee := slogtee.New(slogtee.Options{Redact: func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED:k]") }})
	log := slog.New(tee.Wrap(quiet))
	var c collector
	stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))

	log.Info("pushing to https://x:"+secret+"@example.com/r.git failed",
		"api_token", "anything", "err", errors.New("auth "+secret), "cfg", map[string]string{"v": secret}, "url", "https://u:pw@h/p")
	stop()

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
	raw := got[0].Msg + string(got[0].Attrs)
	for _, leak := range []string{secret, "anything", "u:pw"} {
		if strings.Contains(raw, leak) {
			t.Errorf("%q leaks: %s", leak, raw)
		}
	}
	m := attrs(t, got[0])
	if m["api_token"] != "<redacted>" || m["err"] != "auth [REDACTED:k]" {
		t.Fatalf("attrs = %s", got[0].Attrs)
	}
	if cfg, ok := m["cfg"].(map[string]any); !ok || cfg["v"] != "[REDACTED:k]" {
		t.Fatalf("a structured value keeps its shape: %s", got[0].Attrs)
	}
}

func TestLoggingNeverWaitsForTheSinkAndTheOverflowIsCountedAndReported(t *testing.T) {
	tee := slogtee.New(slogtee.Options{Capacity: 4})
	log := slog.New(tee.Wrap(quiet))
	release := make(chan struct{})
	var c collector
	sink := func(ctx context.Context, es []slogtee.Entry) error {
		<-release
		return c.sink(ctx, es)
	}
	var fallback bytes.Buffer
	stop := run(t, tee, sink, slog.New(slog.NewTextHandler(&fallback, nil)))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			log.Info("line")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging waited for a sink that does not return")
	}
	if tee.Dropped() == 0 {
		t.Fatal("a queue of 4 took 100 lines")
	}
	close(release)
	stop()

	got := c.all()
	var kept, reported float64
	for _, e := range got {
		if e.Level != "warn" {
			kept++
			continue
		}
		n, _ := attrs(t, e)["dropped"].(float64)
		if n < 1 || !strings.Contains(e.Msg, "dropped") {
			t.Fatalf("the overflow entry = %+v", e)
		}
		reported += n
	}
	if reported == 0 || !strings.Contains(fallback.String(), "dropped") {
		t.Fatalf("the overflow is not reported: %d entries, fallback %q", len(got), fallback.String())
	}
	if kept+reported != 100 || uint64(reported) != tee.Dropped() {
		t.Fatalf("%v kept + %v reported != 100 (dropped %d)", kept, reported, tee.Dropped())
	}
}

// The sink logs through the teed logger, as store.WithTx does: nothing waits
// on anything, and the writer stops when asked.
func TestASinkThatLogsThroughTheTeeNeitherDeadlocksNorKeepsTheWriterRunning(t *testing.T) {
	tee := slogtee.New(slogtee.Options{Capacity: 8})
	log := slog.New(tee.Wrap(quiet))
	var c collector
	sink := func(ctx context.Context, es []slogtee.Entry) error {
		log.InfoContext(ctx, "store: a transaction held the writer") // synchronous, from the writer itself
		return c.sink(ctx, es)
	}
	stop := run(t, tee, sink, slog.New(slog.DiscardHandler))
	log.Info("first")
	deadline := time.Now().Add(5 * time.Second)
	for len(c.all()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	if len(c.all()) < 3 {
		t.Fatal("the writer stalled")
	}
}

func TestWhatIsQueuedIsWrittenOnShutdown(t *testing.T) {
	tee := slogtee.New(slogtee.Options{})
	log := slog.New(tee.Wrap(quiet))
	for i := range 1000 {
		log.Info("before the writer", "i", i)
	}
	var c collector
	stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))
	stop()
	if got := len(c.all()); got != 1000 {
		t.Fatalf("%d of 1000 written", got)
	}
	log.Info("after the writer stopped")
	if tee.Dropped() != 0 {
		t.Fatal("a line after the shutdown is not an overflow")
	}
}

func TestAFailingSinkLosesTheBatchAndSaysSoOnTheFallbackOnly(t *testing.T) {
	tee := slogtee.New(slogtee.Options{})
	log := slog.New(tee.Wrap(quiet))
	var fallback bytes.Buffer
	var calls int
	sink := func(context.Context, []slogtee.Entry) error { calls++; return errors.New("disk full") }
	log.Info("a")
	stop := run(t, tee, sink, slog.New(slog.NewTextHandler(&fallback, nil)))
	stop()
	if calls != 1 || tee.Dropped() != 1 || !strings.Contains(fallback.String(), "disk full") {
		t.Fatalf("calls %d, dropped %d, fallback %q", calls, tee.Dropped(), fallback.String())
	}
}

func TestLongMessagesAndAttributesAreBounded(t *testing.T) {
	tee := slogtee.New(slogtee.Options{MaxMessageBytes: 10, MaxAttrsBytes: 64})
	log := slog.New(tee.Wrap(quiet))
	var c collector
	stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))
	log.Info("ñññññññññññ", "big", strings.Repeat("x", 100))
	stop()
	e := c.all()[0]
	if len(e.Msg) > 10 || !strings.HasPrefix("ñññññññññññ", e.Msg) {
		t.Fatalf("msg = %q", e.Msg)
	}
	if len(e.Attrs) > 64 || !json.Valid(e.Attrs) {
		t.Fatalf("attrs = %s", e.Attrs)
	}
}

// A secret held after With was called is still redacted in what With attached,
// whatever JSON has to escape in it.
func TestAttributesOfWithAreRedactedWithTheSecretsHeldAtEmitTime(t *testing.T) {
	const secret = `late"secret\value-123456`
	var mu sync.Mutex
	var held []string
	tee := slogtee.New(slogtee.Options{Redact: func(s string) string {
		mu.Lock()
		defer mu.Unlock()
		for _, h := range held {
			s = strings.ReplaceAll(s, h, "[REDACTED:k]")
		}
		return s
	}})
	child := slog.New(tee.Wrap(quiet)).With("detail", secret).WithGroup("g").With("more", secret)
	var c collector
	stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))

	mu.Lock()
	held = append(held, secret)
	mu.Unlock()
	child.Info("after the hold")
	stop()

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
	m := attrs(t, got[0])
	g, _ := m["g"].(map[string]any)
	if strings.Contains(string(got[0].Attrs), "late") || m["detail"] != "[REDACTED:k]" || g["more"] != "[REDACTED:k]" {
		t.Fatalf("the secret leaks: %s", got[0].Attrs)
	}
}

// A sink that waits for its context cannot hold the shutdown: once the writer
// is asked to stop, the write under way ends drainTimeout later on the clock.
func TestAWriteUnderWayDoesNotHoldTheShutdown(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	tee := slogtee.New(slogtee.Options{Clock: clk})
	log := slog.New(tee.Wrap(quiet))
	entered := make(chan struct{})
	sink := func(ctx context.Context, _ []slogtee.Entry) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); tee.Run(ctx, sink, slog.New(slog.DiscardHandler)) }()

	log.Info("stuck")
	<-entered
	cancel()
	wait, stopWaiting := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWaiting()
	if err := clk.BlockUntil(wait, 2); err != nil { // the write's own timer and the one of the shutdown
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("stopped before the clock moved")
	default:
	}
	clk.Advance(2 * time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer did not stop")
	}
	if tee.Dropped() != 1 {
		t.Fatalf("dropped = %d, want the batch that was cut", tee.Dropped())
	}
}

// Without a stop, a write is bounded by its own deadline.
func TestAWriteIsBoundedByADeadline(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	tee := slogtee.New(slogtee.Options{Clock: clk})
	log := slog.New(tee.Wrap(quiet))
	entered := make(chan struct{})
	sink := func(ctx context.Context, _ []slogtee.Entry) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tee.Run(ctx, sink, slog.New(slog.DiscardHandler))

	log.Info("stuck")
	<-entered
	clk.Advance(time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for tee.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tee.Dropped() != 1 {
		t.Fatal("the write was not cut by its deadline")
	}
}

type nested struct {
	Name   string            `json:"name"`
	Secret string            `json:"client_secret"`
	Inner  *nested           `json:"inner,omitempty"`
	Extra  map[string]string `json:"extra,omitempty"`
}

func TestSensitiveKeysInsideCompositeValuesAreRedactedAtAnyDepth(t *testing.T) {
	const leak = "opaque-value-123456"
	tee := slogtee.New(slogtee.Options{})
	log := slog.New(tee.Wrap(quiet))
	var c collector
	stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))

	log.Info("composite",
		"map", map[string]string{"api_token": leak, "ok": "fine"},
		"struct", nested{Name: "n", Secret: leak},
		"slice", []map[string]any{{"password": leak}, {"n": 42}},
		"deep", map[string]any{"a": []any{map[string]any{"b": nested{Inner: &nested{Extra: map[string]string{"Authorization": leak}}}}}},
	)
	stop()

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
	if strings.Contains(string(got[0].Attrs), leak) {
		t.Fatalf("leaks: %s", got[0].Attrs)
	}
	m := attrs(t, got[0])
	if m["map"].(map[string]any)["ok"] != "fine" {
		t.Fatalf("the rest keeps its value: %s", got[0].Attrs)
	}
	if m["map"].(map[string]any)["api_token"] != "<redacted>" || m["struct"].(map[string]any)["client_secret"] != "<redacted>" {
		t.Fatalf("attrs = %s", got[0].Attrs)
	}
	if s := m["slice"].([]any); s[0].(map[string]any)["password"] != "<redacted>" || s[1].(map[string]any)["n"] != float64(42) {
		t.Fatalf("slice = %s", got[0].Attrs)
	}
}

type groupValuer struct{ leak string }

func (g groupValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("credentials", g.leak), slog.String("plain", "fine"))
}

// Whatever the shape that holds a secret, the persisted bytes do not.
func TestNoShapeOfRecordPersistsASecret(t *testing.T) {
	const leak = "opaque-value-123456"
	const needle = "held-needle-\"quoted\"\n<tag>"
	cases := []struct {
		name string
		log  func(*slog.Logger)
		keep string // what is not a secret and stays
	}{
		{"top-level attribute", func(l *slog.Logger) { l.Info("m", "api_token", leak, "keep", "kept") }, `"keep":"kept"`},
		{"With attribute", func(l *slog.Logger) { l.With("password", leak, "keep", "kept").Info("m") }, `"keep":"kept"`},
		{"WithGroup sensitive", func(l *slog.Logger) { l.WithGroup("token").Info("m", "value", leak, "n", 7) }, ``},
		{"WithGroup sensitive after With", func(l *slog.Logger) {
			l.With("keep", "kept").WithGroup("credentials").With("a", leak).Info("m", "b", leak)
		}, `"keep":"kept"`},
		{"Group sensitive", func(l *slog.Logger) {
			l.Info("m", slog.Group("password", slog.String("value", leak), slog.Group("in", "x", leak)), "keep", "kept")
		}, `"keep":"kept"`},
		{"nested map", func(l *slog.Logger) {
			l.Info("m", "map", map[string]any{"a": map[string]any{"Authorization": leak}, "ok": "fine"})
		}, `"ok":"fine"`},
		{"struct", func(l *slog.Logger) { l.Info("m", "s", nested{Name: "fine", Secret: leak}) }, `"name":"fine"`},
		{"slice of maps", func(l *slog.Logger) {
			l.Info("m", "s", []map[string]any{{"cookie": leak}, {"n": 42}})
		}, `"n":42`},
		{"LogValuer returning a group", func(l *slog.Logger) { l.Info("m", "v", groupValuer{leak}) }, `"plain":"fine"`},
		{"value of a plain key holds a needle", func(l *slog.Logger) {
			l.Info("m", "note", "before "+needle+" after", "map", map[string]string{"k": needle})
		}, `"note":"before `},
		{"needle in a key", func(l *slog.Logger) { l.Info("m", needle, "v") }, ``},
		{"JSON-escaped needle", func(l *slog.Logger) { l.Info("m", "s", struct{ A string }{needle}) }, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tee := slogtee.New(slogtee.Options{Redact: func(s string) string {
				return strings.NewReplacer(leak, "<needle>", needle, "<needle>").Replace(s)
			}})
			var c collector
			stop := run(t, tee, c.sink, slog.New(slog.DiscardHandler))
			tc.log(slog.New(tee.Wrap(quiet)))
			stop()

			got := c.all()
			if len(got) != 1 {
				t.Fatalf("%d entries", len(got))
			}
			b := string(got[0].Attrs)
			for _, bad := range []string{leak, needle, `\"quoted\"`, `<tag`, "<tag>"} {
				if strings.Contains(b, bad) {
					t.Fatalf("persisted %q: %s", bad, b)
				}
			}
			if !json.Valid(got[0].Attrs) {
				t.Fatalf("invalid JSON: %s", b)
			}
			if tc.keep != "" && !strings.Contains(b, tc.keep) {
				t.Fatalf("lost %s: %s", tc.keep, b)
			}
		})
	}
}
