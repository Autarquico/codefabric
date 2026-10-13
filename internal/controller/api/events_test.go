package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// sseFrame is one server-sent event; a comment has only Comment.
type sseFrame struct{ ID, Event, Data, Comment string }

// sseStreamOf reads the frames of a stream on a goroutine, until it ends.
type sseStreamOf struct {
	resp   *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
}

func (s *sseStreamOf) next(t *testing.T) sseFrame {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			t.Fatal("the stream ended")
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a frame")
	}
	return sseFrame{}
}

// nextEvent skips comments and the retry line.
func (s *sseStreamOf) nextEvent(t *testing.T) sseFrame {
	t.Helper()
	for {
		if f := s.next(t); f.Event != "" {
			return f
		}
	}
}

// ended waits for the stream to end and reports whether it did.
func (s *sseStreamOf) ended(wait time.Duration) bool {
	deadline := time.After(wait)
	for {
		select {
		case _, ok := <-s.frames:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func (tc *testController) openStream(hc *http.Client, path string, header http.Header) (*sseStreamOf, int, v1.ErrorBody) {
	tc.t.Helper()
	ctx, cancel := context.WithCancel(tc.t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+tc.addr+path, nil)
	must(tc.t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	must(tc.t, err)
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		var env v1.ErrorEnvelope
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return nil, resp.StatusCode, env.Error
	}
	s := &sseStreamOf{resp: resp, frames: make(chan sseFrame, 256), cancel: cancel}
	go func() {
		defer close(s.frames)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur != (sseFrame{}) {
					s.frames <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, ": "):
				cur.Comment = line[2:]
			case strings.HasPrefix(line, "id: "):
				cur.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.Data = line[6:]
			case strings.HasPrefix(line, "retry: "):
				cur.Comment = line
			}
		}
	}()
	tc.t.Cleanup(cancel)
	return s, resp.StatusCode, v1.ErrorBody{}
}

func (tc *testController) mustStream(hc *http.Client, path string, header http.Header) *sseStreamOf {
	tc.t.Helper()
	s, status, body := tc.openStream(hc, path, header)
	if s == nil {
		tc.t.Fatalf("GET %s = %d %+v\n%s", path, status, body, tc.logs.String())
	}
	return s
}

func isKeepalive(f sseFrame) bool { return f.Comment == "keepalive" }

// blockUntil waits until n timers are armed on the fake clock, in real time and
// for a bounded while: a test that waits for a timer that is never armed fails
// instead of hanging until the timeout of the whole run.
func blockUntil(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntil(ctx, n); err != nil {
		t.Fatalf("no timer was armed on the fake clock: %v", err)
	}
}

// advanceUntil moves the fake clock forward by step, again and again, until the
// stream sends a frame that want accepts, and gives up after ten seconds of real
// time. Where the stream stands when the test starts to advance the clock (it may not
// have armed its timer, or read the clock for the first time) does not matter:
// the clock moves until the frame it is waiting for is due. between runs before each advance.
func (s *sseStreamOf) advanceUntil(t *testing.T, clk *clock.Fake, step time.Duration, want func(sseFrame) bool, between func()) sseFrame {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatal("the stream ended")
			}
			if want(f) {
				return f
			}
		case <-time.After(30 * time.Millisecond):
			if between != nil {
				between()
			}
			clk.Advance(step)
		}
	}
	t.Fatal("the frame never came, however far the clock moved")
	return sseFrame{}
}

func decodeEvent(t *testing.T, f sseFrame) v1.Event {
	t.Helper()
	var e v1.Event
	if f.Event != v1.SSEEvent || json.Unmarshal([]byte(f.Data), &e) != nil {
		t.Fatalf("not an event: %+v", f)
	}
	if f.ID != strconv.FormatUint(e.Sequence, 10) {
		t.Fatalf("id %q for sequence %d", f.ID, e.Sequence)
	}
	return e
}

// makeTasks creates a Task per name, without starting it: each writes one
// TaskCreated event, which carries the name.
func makeTasks(t *testing.T, tc *testController, names ...string) {
	t.Helper()
	if _, err := tc.owner.Project(t.Context(), "backend"); err != nil {
		if _, err := tc.owner.CreateProject(t.Context(), v1.CreateProjectRequest{Name: "backend", Repo: "/srv/backend"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		if _, err := tc.owner.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Name: name, Prompt: "x", NoStart: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func eventsPage(t *testing.T, tc *testController, query string) v1.EventPage {
	t.Helper()
	status, env, raw := tc.call(tc.httpClientFor(tc.ownerDir), http.MethodGet, "/v1/events"+query, "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/events%s = %d %+v", query, status, env)
	}
	var page v1.EventPage
	must(t, json.Unmarshal(raw, &page))
	return page
}

func TestEvents_PagesAndTail(t *testing.T) {
	tc := newController(t, time.Now())
	makeTasks(t, tc, "alpha", "beta")
	if _, err := tc.owner.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Name: "gamma", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA}); err != nil {
		t.Fatal(err)
	}

	all := eventsPage(t, tc, "")
	if len(all.Events) < 3 || all.HasMore || all.Cursor != strconv.FormatUint(all.Events[len(all.Events)-1].Sequence, 10) {
		t.Fatalf("all = %d events, cursor %s, more %v", len(all.Events), all.Cursor, all.HasMore)
	}
	first := eventsPage(t, tc, "?limit=2")
	if len(first.Events) != 2 || !first.HasMore || first.Cursor != strconv.FormatUint(first.Events[1].Sequence, 10) {
		t.Fatalf("first page = %+v", first)
	}
	rest := eventsPage(t, tc, "?limit=500&after="+first.Cursor)
	if len(rest.Events)+2 != len(all.Events) || rest.Events[0].Sequence != all.Events[2].Sequence {
		t.Fatalf("rest = %d events from %d", len(rest.Events), rest.Events[0].Sequence)
	}
	// Past the end the cursor stays where it is.
	if again := eventsPage(t, tc, "?after="+all.Cursor); len(again.Events) != 0 || again.Cursor != all.Cursor {
		t.Fatalf("after the end = %+v", again)
	}

	tail := eventsPage(t, tc, "?tail=2")
	if len(tail.Events) != 2 || tail.Events[1].Sequence != all.Events[len(all.Events)-1].Sequence || tail.Cursor != all.Cursor {
		t.Fatalf("tail = %+v", tail)
	}
	byType := eventsPage(t, tc, "?type=TaskCreated&type=TaskCreated")
	for _, e := range byType.Events {
		if e.Type != domain.EventType("TaskCreated") && e.Type != "TaskCreated" {
			t.Fatalf("type filter let %s through", e.Type)
		}
	}
	if byTask := eventsPage(t, tc, "?task=gamma"); len(byTask.Events) == 0 {
		t.Fatal("no event for the Task")
	}
}

func TestEvents_FiltersAndValidation(t *testing.T) {
	tc := newController(t, time.Now())
	makeTasks(t, tc, "alpha")
	hc := tc.httpClientFor(tc.ownerDir)
	for name, tc2 := range map[string]struct {
		query string
		code  domain.Code
		http  int
	}{
		"cursor ahead of the history": {"?after=9999", domain.CodeCursorExpired, 409},
		"cursor not a number":         {"?after=abc", domain.CodeInvalidRequest, 400},
		"negative limit":              {"?limit=-1", domain.CodeInvalidRequest, 400},
		"limit above the maximum":     {"?limit=501", domain.CodeInvalidRequest, 400},
		"tail above the maximum":      {"?tail=501", domain.CodeInvalidRequest, 400},
		"tail with after":             {"?tail=2&after=1", domain.CodeInvalidRequest, 400},
		"bad level":                   {"?level=loud", domain.CodeInvalidRequest, 400},
		"bad since":                   {"?since=yesterday", domain.CodeInvalidRequest, 400},
		"unknown project":             {"?project=nowhere", domain.CodeNotFound, 404},
		"unknown worker":              {"?worker=nowhere", domain.CodeNotFound, 404},
		"type with a space":           {"?type=Run%20Queued", domain.CodeInvalidRequest, 400},
	} {
		status, env, _ := tc.call(hc, http.MethodGet, "/v1/events"+tc2.query, "")
		if status != tc2.http || env.Code != tc2.code {
			t.Errorf("%s: %d %s, want %d %s", name, status, env.Code, tc2.http, tc2.code)
		}
	}
	types := ""
	for range v1.EventsMaxTypes + 1 {
		types += "&type=TaskCreated"
	}
	if status, env, _ := tc.call(hc, http.MethodGet, "/v1/events?"+types[1:], ""); status != 400 || env.Code != domain.CodeInvalidRequest {
		t.Errorf("too many types: %d %s", status, env.Code)
	}
	if page := eventsPage(t, tc, "?level=error"); len(page.Events) != 0 {
		t.Errorf("level=error: %+v", page.Events)
	}
	if page := eventsPage(t, tc, "?since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); len(page.Events) != 0 {
		t.Errorf("since the future: %+v", page.Events)
	}
	if page := eventsPage(t, tc, "?since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)); len(page.Events) == 0 {
		t.Error("since an hour ago: nothing")
	}
}

// The authorisation matrix: who may read events, on both routes.
func TestEvents_OnlyClientsRead(t *testing.T) {
	tc := newController(t, time.Now())
	worker := tc.enrollWorker()
	for _, path := range []string{"/v1/events", "/v1/events/stream"} {
		if status, env, _ := tc.call(worker, http.MethodGet, path, ""); status != http.StatusForbidden || env.Code != domain.CodePolicyDenied {
			t.Errorf("a Worker on %s: %d %s", path, status, env.Code)
		}
		if status, env, _ := tc.call(tc.httpClient(), http.MethodGet, path, ""); status != http.StatusUnauthorized || env.Code != domain.CodeUnauthenticated {
			t.Errorf("no certificate on %s: %d %s", path, status, env.Code)
		}
	}
}

func TestEventStream_DeliversAsItHappensAndResumesWithoutGapsOrDuplicates(t *testing.T) {
	tc := newController(t, time.Now())
	hc := tc.httpClientFor(tc.ownerDir)
	s := tc.mustStream(hc, "/v1/events/stream", nil)
	if f := s.next(t); !strings.HasPrefix(f.Comment, "retry: 2000") {
		t.Fatalf("first frame = %+v", f)
	}
	makeTasks(t, tc, "one", "two")
	var last uint64
	for range 2 {
		e := decodeEvent(t, s.nextEvent(t))
		if e.Type != "TaskCreated" || e.Sequence <= last {
			t.Fatalf("event = %+v", e)
		}
		last = e.Sequence
	}
	s.cancel()

	// While the client is away, more happens; it comes back with Last-Event-ID.
	makeTasks(t, tc, "three", "four")
	again := tc.mustStream(hc, "/v1/events/stream", http.Header{"Last-Event-Id": {strconv.FormatUint(last, 10)}})
	var got []string
	for range 2 {
		e := decodeEvent(t, again.nextEvent(t))
		if e.Sequence <= last {
			t.Fatalf("a duplicate: %d after %d", e.Sequence, last)
		}
		last = e.Sequence
		got = append(got, e.Data["name"].(string)) //nolint:forcetypeassert // the event carries the name
	}
	if strings.Join(got, ",") != "three,four" {
		t.Fatalf("resumed with %v", got)
	}
}

func TestEventStream_LastEventIDWinsOverAfterAndAMalformedOneIsRefused(t *testing.T) {
	tc := newController(t, time.Now())
	makeTasks(t, tc, "one", "two", "three")
	hc := tc.httpClientFor(tc.ownerDir)
	all := eventsPage(t, tc, "")

	s := tc.mustStream(hc, "/v1/events/stream?after=0", http.Header{"Last-Event-Id": {strconv.FormatUint(all.Events[1].Sequence, 10)}})
	if e := decodeEvent(t, s.nextEvent(t)); e.Sequence != all.Events[2].Sequence {
		t.Fatalf("Last-Event-ID did not win: first event %d, want %d", e.Sequence, all.Events[2].Sequence)
	}
	s2 := tc.mustStream(hc, "/v1/events/stream?after="+strconv.FormatUint(all.Events[0].Sequence, 10), nil)
	if e := decodeEvent(t, s2.nextEvent(t)); e.Sequence != all.Events[1].Sequence {
		t.Fatalf("after: first event %d, want %d", e.Sequence, all.Events[1].Sequence)
	}
	// From now: nothing of the past.
	s3 := tc.mustStream(hc, "/v1/events/stream", nil)
	select {
	case f := <-s3.frames:
		if f.Event != "" {
			t.Fatalf("from now delivered %+v", f)
		}
	case <-time.After(200 * time.Millisecond):
	}

	for name, id := range map[string]string{"text": "abc", "negative": "-1", "float": "1.5", "huge": "99999999999999999999"} {
		_, status, env := tc.openStream(hc, "/v1/events/stream", http.Header{"Last-Event-Id": {id}})
		if status != 400 || env.Code != domain.CodeInvalidRequest {
			t.Errorf("Last-Event-ID %s: %d %s", name, status, env.Code)
		}
	}
	// A cursor ahead of the history is permanent: a plain error before the first byte.
	_, status, env := tc.openStream(hc, "/v1/events/stream", http.Header{"Last-Event-Id": {"99999"}})
	if status != 409 || env.Code != domain.CodeCursorExpired {
		t.Errorf("a cursor ahead: %d %s", status, env.Code)
	}
	if _, status, env = tc.openStream(hc, "/v1/events/stream?level=loud", nil); status != 400 || env.Code != domain.CodeInvalidRequest {
		t.Errorf("a bad filter: %d %s", status, env.Code)
	}
}

func TestEventStream_AFilteredStreamStillDeliversWhatMatches(t *testing.T) {
	tc := newController(t, time.Now())
	hc := tc.httpClientFor(tc.ownerDir)
	s := tc.mustStream(hc, "/v1/events/stream?type=RunQueued&level=info", nil)
	makeTasks(t, tc, "noise-1", "noise-2")
	select {
	case f := <-s.frames:
		if f.Event != "" {
			t.Fatalf("a TaskCreated got through a RunQueued filter: %+v", f)
		}
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := tc.owner.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Name: "runner", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA}); err != nil {
		t.Fatal(err)
	}
	if e := decodeEvent(t, s.nextEvent(t)); e.Type != "RunQueued" {
		t.Fatalf("event = %+v", e)
	}
}

func TestEventStream_KeepsAliveWhileQuietAndAcrossTheServerTimeouts(t *testing.T) {
	// The server's own read and write timeouts are far shorter than the stream.
	tc := newController(t, time.Now(), func(c *api.Config) {
		c.Timeouts = api.Timeouts{Read: 300 * time.Millisecond, Write: 300 * time.Millisecond}
	})
	hc := tc.httpClientFor(tc.ownerDir)
	s := tc.mustStream(hc, "/v1/events/stream", nil)
	s.next(t) // retry
	time.Sleep(900 * time.Millisecond)
	if f := s.advanceUntil(t, tc.clock, 15*time.Second, isKeepalive, nil); f.Comment != "keepalive" {
		t.Fatalf("frame = %+v, want a keepalive", f)
	}
	makeTasks(t, tc, "late")
	if e := decodeEvent(t, s.nextEvent(t)); e.Data["name"] != "late" {
		t.Fatalf("event = %+v", e)
	}
	// Unrelated events do not postpone the keepalive of a filtered stream.
	// If every wake restarted the count, six seconds at a time would never add up to fifteen.
	f := tc.mustStream(hc, "/v1/events/stream?type=NoSuchEvent", nil)
	f.next(t)
	n := 0
	wake := func() { n++; makeTasks(t, tc, "n"+strconv.Itoa(n)) }
	if fr := f.advanceUntil(t, tc.clock, 6*time.Second, isKeepalive, wake); fr.Comment != "keepalive" {
		t.Fatalf("filtered stream frame = %+v", fr)
	}
}

func TestEventStream_EndsCleanlyAtItsLifetimeAndTheClientResumes(t *testing.T) {
	tc := newController(t, time.Now(), func(c *api.Config) { c.Timeouts = api.Timeouts{StreamLifetime: 400 * time.Millisecond} })
	hc := tc.httpClientFor(tc.ownerDir)
	s := tc.mustStream(hc, "/v1/events/stream", nil)
	makeTasks(t, tc, "before")
	last := decodeEvent(t, s.nextEvent(t)).Sequence
	if !s.ended(5 * time.Second) {
		t.Fatal("the stream outlived its lifetime")
	}
	makeTasks(t, tc, "after")
	again := tc.mustStream(hc, "/v1/events/stream", http.Header{"Last-Event-Id": {strconv.FormatUint(last, 10)}})
	if e := decodeEvent(t, again.nextEvent(t)); e.Data["name"] != "after" {
		t.Fatalf("resumed with %+v", e)
	}
}

func TestEventStream_ARevokedIdentityGetsAnErrorEventAndTheStreamEnds(t *testing.T) {
	tc := newController(t, time.Now())
	laptop, dir := tc.enroll("laptop")
	hc := tc.httpClientFor(dir)
	s := tc.mustStream(hc, "/v1/events/stream", nil)
	s.next(t)
	clients, err := tc.owner.Clients(t.Context())
	must(t, err)
	for _, c := range clients {
		if c.Name == "laptop" {
			must(t, tc.owner.RemoveClient(t.Context(), c.ID))
		}
	}
	_ = laptop
	// The stream looks again when something happens, and no longer finds its caller.
	makeTasks(t, tc, "wakes-it")
	f := s.nextEvent(t)
	var body v1.StreamError
	if f.Event != v1.SSEError || json.Unmarshal([]byte(f.Data), &body) != nil || body.Code != domain.CodeUnauthenticated {
		t.Fatalf("frame = %+v", f)
	}
	if !s.ended(5 * time.Second) {
		t.Fatal("the stream did not end after the error")
	}
}

func TestEventStream_AKeepaliveAuthorisesAgain(t *testing.T) {
	tc := newController(t, time.Now())
	_, dir := tc.enroll("laptop")
	s := tc.mustStream(tc.httpClientFor(dir), "/v1/events/stream", nil)
	s.next(t)
	// The stream has looked once and waits on its keepalive: only then is the
	// identity revoked, so that it is the keepalive that finds it out, not the first look.
	blockUntil(t, tc.clock, 1)
	clients, err := tc.owner.Clients(t.Context())
	must(t, err)
	for _, c := range clients {
		if c.Name == "laptop" {
			must(t, tc.owner.RemoveClient(t.Context(), c.ID))
		}
	}
	var body v1.StreamError
	if f := s.advanceUntil(t, tc.clock, 15*time.Second, func(f sseFrame) bool { return f.Event == v1.SSEError }, nil); f.Event != v1.SSEError || json.Unmarshal([]byte(f.Data), &body) != nil || body.Code != domain.CodeUnauthenticated {
		t.Fatalf("frame = %+v", f)
	}
}

func TestEventStream_QuotasPerIdentityAndInTotalAreGivenBack(t *testing.T) {
	tc := newController(t, time.Now(), func(c *api.Config) { c.Streams = api.StreamLimits{PerIdentity: 2, Total: 3} })
	hc := tc.httpClientFor(tc.ownerDir)
	a := tc.mustStream(hc, "/v1/events/stream", nil)
	b := tc.mustStream(hc, "/v1/events/stream", nil)
	_, status, env := tc.openStream(hc, "/v1/events/stream", nil)
	if status != http.StatusTooManyRequests || env.Code != domain.CodeInProgress {
		t.Fatalf("third stream of one identity: %d %s", status, env.Code)
	}
	// A cancelled stream gives its place back.
	a.cancel()
	waitFor(t, func() bool {
		s, _, _ := tc.openStream(hc, "/v1/events/stream", nil)
		if s != nil {
			s.cancel()
		}
		return s != nil
	})

	// The total is shared by the identities.
	_, dir := tc.enroll("laptop")
	other := tc.httpClientFor(dir)
	c := tc.mustStream(other, "/v1/events/stream", nil)
	waitFor(t, func() bool {
		_, status, _ := tc.openStream(other, "/v1/events/stream", nil)
		return status == http.StatusTooManyRequests
	})
	b.cancel()
	c.cancel()

	// A stream that fails before its first byte gives its place back too.
	for range 5 {
		if _, status, _ := tc.openStream(hc, "/v1/events/stream?level=loud", nil); status != 400 {
			t.Fatalf("bad filter: %d", status)
		}
	}
	waitFor(t, func() bool {
		s, _, _ := tc.openStream(hc, "/v1/events/stream", nil)
		if s != nil {
			s.cancel()
		}
		return s != nil
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
	}
}

func TestEventStream_ShutdownEndsTheStreamsAndRefusesNewOnes(t *testing.T) {
	tc := newController(t, time.Now())
	hc := tc.httpClientFor(tc.ownerDir)
	s := tc.mustStream(hc, "/v1/events/stream", nil)
	s.next(t)
	started := time.Now()
	tc.stop()
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("shutdown took %s with a stream open: it waited for its timeout", took)
	}
	if !s.ended(5 * time.Second) {
		t.Fatal("the stream survived the shutdown")
	}
}
