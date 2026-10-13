package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// testClient is a client against a TLS test server that answers with h, with
// quick timings on the real clock.
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	c := &Client{base: srv.URL, controller: srv.Listener.Addr().String(), clock: clock.Real{}, recovery: renewHint}
	c.timing = streamTiming{header: 5 * time.Second, idle: 5 * time.Second, backoffInitial: 5 * time.Millisecond, backoffMax: 20 * time.Millisecond}
	base := srv.Client()
	c.http.Store(base)
	c.stream.Store(&http.Client{Transport: base.Transport})
	return c
}

func sseWrite(w http.ResponseWriter, text string) {
	_, _ = fmt.Fprint(w, text)
	w.(http.Flusher).Flush()
}

func eventFrame(seq uint64) string {
	raw, _ := json.Marshal(v1.Event{Sequence: seq, Type: "TaskCreated", Message: "m"})
	return fmt.Sprintf("id: %d\nevent: event\ndata: %s\n\n", seq, raw)
}

func TestReadSSE_ParsesFramesAndDropsACutOne(t *testing.T) {
	text := ": comment\nretry: 2000\n\nid: 1\nevent: a\ndata: x\ndata: y\n\nevent: b\ndata: {}\n\nid: 9\nevent: cut\ndata: par"
	var got []sseEvent
	activity := 0
	if err := readSSE(strings.NewReader(text), func() { activity++ }, func(ev sseEvent) error { got = append(got, ev); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (sseEvent{ID: "1", Name: "a", Data: "x\ny"}) || got[1] != (sseEvent{Name: "b", Data: "{}"}) {
		t.Fatalf("events = %+v", got)
	}
	if activity < 5 {
		t.Errorf("activity = %d", activity)
	}
	boom := errors.New("boom")
	if err := readSSE(strings.NewReader("event: a\ndata: x\n\n"), func() {}, func(sseEvent) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("fn error = %v", err)
	}
	if err := readSSE(strings.NewReader("data: "+strings.Repeat("x", maxSSELine+10)+"\n\n"), func() {}, func(sseEvent) error { return nil }); err == nil {
		t.Error("a line over the limit was accepted")
	}
}

func TestFollowEvents_ReconnectsWithTheLastEventIDWithoutGapsOrDuplicates(t *testing.T) {
	var (
		mu      sync.Mutex
		resumes []string
	)
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			_ = json.NewEncoder(w).Encode(v1.EventPage{Events: []v1.Event{}, Cursor: "10"})
			return
		}
		mu.Lock()
		resumes = append(resumes, r.Header.Get("Last-Event-ID"))
		n := len(resumes)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		switch n {
		case 1:
			sseWrite(w, eventFrame(11)+eventFrame(12)+"id: 13\nevent: event\ndata: {\"sequ") // cut in the middle of an event
		case 2:
			sseWrite(w, eventFrame(13)+eventFrame(14))
		default:
			<-r.Context().Done()
		}
	}))
	var got []uint64
	ctx, cancel := context.WithCancel(t.Context())
	err := c.FollowEvents(ctx, EventQuery{Types: []string{"TaskCreated"}}, func(e v1.Event) error {
		got = append(got, e.Sequence)
		if e.Sequence == 14 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(got) != "[11 12 13 14]" {
		t.Fatalf("delivered %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(resumes) != "[10 12]" {
		t.Fatalf("resumed with %q, want from the cursor and then from 12", resumes)
	}
}

func TestFollowEvents_AFailingCallbackIsNotRetried(t *testing.T) {
	var connections atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			_ = json.NewEncoder(w).Encode(v1.EventPage{Cursor: "0"})
			return
		}
		connections.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		sseWrite(w, eventFrame(1))
		<-r.Context().Done()
	}))
	boom := errors.New("the consumer broke")
	err := c.FollowEvents(t.Context(), EventQuery{After: "0"}, func(v1.Event) error { return boom })
	if !errors.Is(err, boom) || connections.Load() != 1 {
		t.Fatalf("err = %v after %d connections", err, connections.Load())
	}
}

func TestFollowEvents_PermanentErrorsEndItAndTheOthersAreRetried(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serve     func(w http.ResponseWriter)
		permanent domain.Code
	}{
		{"unauthenticated event", errorEvent(domain.CodeUnauthenticated), domain.CodeUnauthenticated},
		{"policy denied event", errorEvent(domain.CodePolicyDenied), domain.CodePolicyDenied},
		{"invalid request event", errorEvent(domain.CodeInvalidRequest), domain.CodeInvalidRequest},
		{"not found event", errorEvent(domain.CodeNotFound), domain.CodeNotFound},
		{"cursor expired event", errorEvent(domain.CodeCursorExpired), domain.CodeCursorExpired},
		{"cursor expired status", status(http.StatusConflict, domain.CodeCursorExpired), domain.CodeCursorExpired},
		{"forbidden status", status(http.StatusForbidden, domain.CodePolicyDenied), domain.CodePolicyDenied},
	} {
		var connections atomic.Int32
		c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			connections.Add(1)
			tc.serve(w)
		}))
		err := c.FollowEvents(t.Context(), EventQuery{After: "5"}, func(v1.Event) error { return nil })
		if !isCode(err, tc.permanent) || connections.Load() != 1 {
			t.Errorf("%s: err = %v after %d connections", tc.name, err, connections.Load())
		}
	}

	// Not permanent: an internal error event, a 503, a 429. The third try works.
	var tries atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch tries.Add(1) {
		case 1:
			errorEvent(domain.CodeInternal)(w)
		case 2:
			status(http.StatusServiceUnavailable, domain.CodeControllerUnreachable)(w)
		case 3:
			status(http.StatusTooManyRequests, domain.CodeInProgress)(w)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w, eventFrame(6))
			<-r.Context().Done()
		}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	err := c.FollowEvents(ctx, EventQuery{After: "5"}, func(v1.Event) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || tries.Load() != 4 {
		t.Fatalf("err = %v after %d tries", err, tries.Load())
	}
}

func errorEvent(code domain.Code) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		raw, _ := json.Marshal(v1.StreamError{Code: code, Message: "no"})
		sseWrite(w, "event: error\ndata: "+string(raw)+"\n\n")
	}
}

func status(code int, c domain.Code) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v1.ErrorEnvelope{Error: v1.ErrorBody{Code: c, Message: "no"}})
	}
}

func TestFollowEvents_ASilentConnectionIsAbandonedAndAnUnansweredOneToo(t *testing.T) {
	var (
		mu    sync.Mutex
		opens int
	)
	srv := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		opens++
		n := opens
		mu.Unlock()
		switch n {
		case 1: // never answers
			<-r.Context().Done()
		case 2: // answers and goes silent
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w, ": retry\n\n")
			<-r.Context().Done()
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w, eventFrame(1))
			<-r.Context().Done()
		}
	}
	c := testClient(t, http.HandlerFunc(srv))
	fake := clock.NewFake(time.Unix(0, 0))
	c.clock = fake
	c.timing = streamTiming{header: 30 * time.Second, idle: 45 * time.Second, backoffInitial: time.Second, backoffMax: 10 * time.Second}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- c.FollowEvents(ctx, EventQuery{After: "0"}, func(v1.Event) error { cancel(); return nil })
	}()
	advance := func(d time.Duration) {
		t.Helper()
		if err := fake.BlockUntil(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		fake.Advance(d)
	}
	waitOpens := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			got := opens
			mu.Unlock()
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("only %d connections", got)
			}
		}
	}
	waitOpens(1)
	advance(30 * time.Second) // the header timer
	advance(time.Second)      // the backoff
	waitOpens(2)
	time.Sleep(50 * time.Millisecond)
	advance(44 * time.Second) // a keepalive's worth of silence is not yet the end
	mu.Lock()
	if opens != 2 {
		t.Errorf("reconnected after 44 s of silence")
	}
	mu.Unlock()
	advance(time.Second) // 45 s
	advance(2 * time.Second)
	waitOpens(3)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestFollowOutput_ResumesAtTheOffsetOfTheBeginAndEndsWithEOF(t *testing.T) {
	var (
		mu      sync.Mutex
		resumes []string
	)
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		resumes = append(resumes, r.Header.Get("Last-Event-ID"))
		n := len(resumes)
		mu.Unlock()
		if r.URL.Query().Get("follow") != "true" || r.URL.Query().Get("tail") != "10" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1: // begins, then drops before any chunk
			sseWrite(w, `event: begin`+"\n"+`data: {"run_id":"run_x","lease_epoch":2,"start":0,"end":50,"offset":40}`+"\n\n")
		case 2: // delivers a chunk, then drops
			sseWrite(w, `event: begin`+"\n"+`data: {"run_id":"run_x","lease_epoch":2,"start":0,"end":50,"offset":40}`+"\n\n"+
				"id: 2:45\nevent: output\ndata: "+`{"run_id":"run_x","lease_epoch":2,"offset":40,"data":"aGVsbG8="}`+"\n\n")
		default:
			sseWrite(w, `event: begin`+"\n"+`data: {"run_id":"run_x","lease_epoch":2,"start":0,"end":60,"offset":45}`+"\n\n"+
				"id: 2:50\nevent: output\ndata: "+`{"run_id":"run_x","lease_epoch":2,"offset":45,"data":"d29ybGQ="}`+"\n\n"+
				"event: eof\ndata: "+`{"run_id":"run_x","lease_epoch":2,"final_offset":50}`+"\n\n")
		}
	}))
	var got strings.Builder
	tail := uint64(10)
	ending, err := c.FollowOutput(t.Context(), "run-1", OutputQuery{Tail: &tail}, OutputHandler{
		Begin: func(v1.OutputBegin) error { return nil },
		Chunk: func(ch v1.OutputChunk) error { got.Write(ch.Data); return nil },
	})
	if err != nil || ending.EOF == nil || ending.EOF.FinalOffset != 50 || ending.End != nil {
		t.Fatalf("ending = %+v, %v", ending, err)
	}
	if got.String() != "helloworld" {
		t.Fatalf("delivered %q", got.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(resumes) != "[ 2:40 2:45]" {
		t.Fatalf("resumed with %q", resumes)
	}
}

func TestFollowOutput_NewerEpochEndsItAndAnErrorEventStaysAnError(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseWrite(w, "event: end\ndata: "+`{"run_id":"run_x","reason":"newer_epoch","lease_epoch":3}`+"\n\n")
	}))
	ending, err := c.FollowOutput(t.Context(), "run-1", OutputQuery{}, OutputHandler{
		Begin: func(v1.OutputBegin) error { return nil }, Chunk: func(v1.OutputChunk) error { return nil },
	})
	if err != nil || ending.End == nil || ending.End.Reason != v1.EndNewerEpoch || ending.End.LeaseEpoch != 3 {
		t.Fatalf("ending = %+v, %v", ending, err)
	}
}

func TestRequestsOfTheObservationEndpoints(t *testing.T) {
	var path string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.RequestURI()
		switch {
		case strings.HasSuffix(r.URL.Path, "/attach"):
			_ = json.NewEncoder(w).Encode(v1.AttachTarget{RunID: "run_x"})
		case r.URL.Path == "/v1/ps":
			_ = json.NewEncoder(w).Encode(v1.PsResponse{})
		case r.URL.Path == "/v1/worker/output":
			_ = json.NewEncoder(w).Encode(v1.OutputBatchResponse{Acks: []v1.OutputAck{{RunID: "run_x", End: 3}}})
		case strings.HasSuffix(r.URL.Path, "/output"):
			_ = json.NewEncoder(w).Encode(v1.RunOutput{RunID: "run_x", Data: []byte("hi")})
		default:
			_ = json.NewEncoder(w).Encode(v1.EventPage{Cursor: "7"})
		}
	}))
	tail, offset := uint64(5), uint64(9)
	for want, call := range map[string]func() error{
		"GET /v1/runs/auth-race/attach?project=backend": func() error { _, err := c.AttachTarget(t.Context(), "auth-race", "backend"); return err },
		"GET /v1/ps?project=backend":                    func() error { _, err := c.Ps(t.Context(), "backend"); return err },
		"POST /v1/worker/output":                        func() error { _, err := c.UploadOutput(t.Context(), v1.OutputBatch{}); return err },
		"GET /v1/runs/run-1/output?epoch=2&limit=100&tail=5": func() error {
			out, err := c.RunOutput(t.Context(), "run-1", OutputQuery{Epoch: 2, Tail: &tail, Limit: 100})
			if err == nil && string(out.Data) != "hi" {
				err = errors.New("data")
			}
			return err
		},
		"GET /v1/runs/run-1/output?offset=9": func() error { _, err := c.RunOutput(t.Context(), "run-1", OutputQuery{Offset: &offset}); return err },
		"GET /v1/events?after=3&level=warn&limit=2&run=run-1&since=2026-10-10T12%3A00%3A00Z&task=t&type=A&type=B&worker=w&project=p": func() error {
			_, err := c.Events(t.Context(), EventQuery{After: "3", Limit: 2, Project: "p", Task: "t", Run: "run-1", Worker: "w", Types: []string{"A", "B"}, Level: "warn", Since: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)})
			return err
		},
	} {
		if err := call(); err != nil {
			t.Errorf("%s: %v", want, err)
		}
		// Query keys are sorted by Encode, so compare as sets.
		if !sameRequest(path, want) {
			t.Errorf("request = %s, want %s", path, want)
		}
	}
}

func sameRequest(got, want string) bool {
	gp, gq, _ := strings.Cut(got, "?")
	wp, wq, _ := strings.Cut(want, "?")
	if gp != wp {
		return false
	}
	g, w := strings.Split(gq, "&"), strings.Split(wq, "&")
	if len(g) != len(w) {
		return false
	}
	set := map[string]int{}
	for _, x := range g {
		set[x]++
	}
	for _, x := range w {
		set[x]--
	}
	for _, n := range set {
		if n != 0 {
			return false
		}
	}
	return true
}
