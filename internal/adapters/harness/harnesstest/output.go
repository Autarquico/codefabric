package harnesstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// eventBound is how long a check waits for Events to deliver or close.
const eventBound = 5 * time.Second

// recorded is the handle of a Run whose output the fixture recorded.
func (e *env) recorded(t *testing.T) harness.Handle {
	t.Helper()
	h := handleOf(e.runSpec(t, e.mode()))
	e.f.Recorded(t, h)
	return h
}

func source(h harness.Handle) string {
	if h.EventsPath != "" {
		return h.EventsPath
	}
	return h.OutputPath
}

func checkCheckpoint(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	h := e.recorded(t)
	var cp harness.Checkpoint
	var err error
	unchanged(t, "Checkpoint", h.RunDir, func() {
		e.homeUnchanged(t, "Checkpoint", func() { cp, err = e.a.Checkpoint(t.Context(), e.host, h) })
	})
	if !e.caps.NativeCheckpoint {
		isUnsupported(t, "Checkpoint", cp, err)
		return
	}
	if err != nil {
		t.Fatalf("NativeCheckpoint is declared but Checkpoint: %v", err)
	}
	if cp.Harness != e.a.ID() || cp.Format == "" || len(cp.Data) == 0 {
		t.Errorf("Checkpoint = %+v, want the ID of the adapter, a format and data", cp)
	}
	if again, err := e.a.Checkpoint(t.Context(), e.host, h); err != nil || !reflect.DeepEqual(again, cp) {
		t.Errorf("Checkpoint of the same Run gave %+v, then %+v, %v", cp, again, err)
	}
}

func checkInput(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	run := e.runSpec(t, e.mode())
	var err error
	if !e.caps.StructuredInput {
		unchanged(t, "an unsupported SendInput", run.RunDir, func() {
			err = e.a.SendInput(t.Context(), e.host, handleOf(run), harness.Input{Text: "harnesstest"})
		})
		isUnsupported(t, "SendInput", nil, err)
		return
	}
	e.homeUnchanged(t, "SendInput", func() {
		err = e.a.SendInput(t.Context(), e.host, handleOf(run), harness.Input{Text: "harnesstest"})
	})
	if err != nil {
		t.Errorf("StructuredInput is declared but SendInput: %v", err)
	}
}

func checkUsage(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	h := e.recorded(t)
	var usage []harness.UsageEvent
	var err error
	e.homeUnchanged(t, "CollectUsage", func() { usage, err = e.a.CollectUsage(t.Context(), e.host, h) })
	if !e.caps.UsageEvents {
		isUnsupported(t, "CollectUsage", usage, err)
		return
	}
	requireUsageFixture(t, f.ExpectUsage)
	if err != nil {
		t.Fatalf("UsageEvents is declared but CollectUsage: %v", err)
	}
	if !reflect.DeepEqual(usage, f.ExpectUsage) {
		t.Errorf("CollectUsage =\n%s\nwant\n%s", usageString(usage), usageString(f.ExpectUsage))
	}
}

// requireUsageFixture fails t unless want tells unknown, a known zero and a
// known non-zero apart, which a parser that guesses would confuse.
func requireUsageFixture(t *testing.T, want []harness.UsageEvent) {
	t.Helper()
	var unknown, zero, known bool
	for _, u := range want {
		for _, p := range usageFields(u) {
			switch {
			case p == nil:
				unknown = true
			case *p == 0:
				zero = true
			default:
				known = true
			}
		}
	}
	if !unknown || !zero || !known {
		t.Fatal("harnesstest: ExpectUsage needs an unknown (nil) field, a known zero and a known non-zero")
	}
}

func usageFields(u harness.UsageEvent) []*int64 {
	return []*int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.RequestCount, u.LatencyMS}
}

func usageString(us []harness.UsageEvent) string {
	if us == nil {
		return "  nil"
	}
	var b strings.Builder
	for _, u := range us {
		fields := make([]string, 0, 6)
		for i, p := range usageFields(u) {
			v := "?"
			if p != nil {
				v = fmt.Sprint(*p)
			}
			fields = append(fields, []string{"in", "out", "cache_read", "cache_write", "requests", "latency_ms"}[i]+"="+v)
		}
		fmt.Fprintf(&b, "  %s %s %s %s\n", u.At.Format(time.RFC3339), u.Provider, u.Model, strings.Join(fields, " "))
	}
	return b.String()
}

// readCloser is a source of Events that tells when it is closed.
type readCloser struct {
	io.Reader
	closed bool
}

func (r *readCloser) Close() error { r.closed = true; return nil }

// collect reads ch until it closes, failing t if it does not within
// eventBound.
func collect(t *testing.T, ch <-chan harness.Event) []harness.Event {
	t.Helper()
	var evs []harness.Event
	timeout := time.After(eventBound)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatalf("the events channel did not close within %s; got %d events", eventBound, len(evs))
		}
	}
}

func checkEvents(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	h := e.recorded(t)
	b, err := os.ReadFile(source(h))
	if err != nil {
		t.Fatalf("the fixture recorded nothing at %s: %v", source(h), err)
	}
	src := &readCloser{Reader: bytes.NewReader(b)}
	ch, err := e.a.Events(t.Context(), h, src)
	if errors.Is(err, harness.ErrUnsupported) {
		isUnsupported(t, "Events", ch, err)
		if e.caps.UsageEvents || e.caps.NeedsInputSignal {
			t.Error("UsageEvents or NeedsInputSignal is declared but Events is unsupported")
		}
		return
	}
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	evs := collect(t, ch)
	if src.closed {
		t.Error("Events closed its source, which belongs to the caller")
	}
	if !reflect.DeepEqual(evs, f.ExpectEvents) {
		t.Errorf("Events =\n%s\nwant\n%s", describe(evs), describe(f.ExpectEvents))
	}
	for kind, declared := range map[harness.EventKind]bool{harness.EventUsage: e.caps.UsageEvents, harness.EventInputRequired: e.caps.NeedsInputSignal} {
		has := slices.ContainsFunc(evs, func(ev harness.Event) bool { return ev.Kind == kind })
		switch {
		case declared && !has:
			t.Errorf("the capability of %s events is declared but the recording gave none", kind)
		case !declared && has:
			t.Errorf("the capability of %s events is not declared but Events gave one", kind)
		}
	}
	garbage := "not json\n{\n[]\n{}\nnull\n\"usage\"\n{\"type\":\"harnesstest\"}\n\x00\x01\x02\n"
	ch, err = e.a.Events(t.Context(), h, &readCloser{Reader: strings.NewReader(garbage)})
	if err != nil {
		t.Fatalf("Events over garbage: %v", err)
	}
	if evs := collect(t, ch); len(evs) > 0 {
		t.Errorf("Events made up %s out of garbage", describe(evs))
	}
}

func checkEventsCancel(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	h := e.recorded(t)
	recording, err := os.ReadFile(source(h))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("blocked read", func(t *testing.T) {
		base := runtime.NumGoroutine()
		pr, pw := io.Pipe()
		ctx, cancel := context.WithCancel(t.Context())
		ch, err := e.a.Events(ctx, h, pr)
		if errors.Is(err, harness.ErrUnsupported) {
			cancel()
			return
		}
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		collect(t, ch) // with the read still blocked: the source is not closed yet
		_ = pr.Close()
		_ = pw.Close()
		goroutinesBack(t, base)
	})
	t.Run("blocked send", func(t *testing.T) {
		base := runtime.NumGoroutine()
		src := &readCloser{Reader: bytes.NewReader(bytes.Repeat(recording, 1000))}
		ctx, cancel := context.WithCancel(t.Context())
		ch, err := e.a.Events(ctx, h, src)
		if errors.Is(err, harness.ErrUnsupported) {
			cancel()
			return
		}
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		select {
		case <-ch:
		case <-time.After(eventBound):
			t.Errorf("no event within %s", eventBound)
		}
		cancel()
		_ = src.Close()
		goroutinesBack(t, base)
		collect(t, ch)
	})
}

// goroutinesBack fails t unless the goroutines are back to base within
// eventBound: what Events started has returned.
func goroutinesBack(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(eventBound)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Errorf("Events left %d goroutines running after its ctx ended and its source was closed", runtime.NumGoroutine()-base)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
