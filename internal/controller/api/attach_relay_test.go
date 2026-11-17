package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/attachhub"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const attachWait = 10 * time.Second

// attachFixture is a Controller with an interactive Run that is running on
// "alpha", a Worker that announces the relay, and clients to attach with. The
// Worker end of every session is played by the test through the Worker SDK.
type attachFixture struct {
	*testController
	work          *app.Services
	alpha, beta   *client.Client
	alphaD, betaD string
	alphaID       domain.ID
	laptop        *client.Client
	laptopID      string
	run           v1.Run
}

func relayHeartbeat(withRelay bool) v1.HeartbeatRequest {
	hb := heartbeat(newEpoch())
	if withRelay {
		hb.Capabilities = append(hb.Capabilities, domain.AttachRelayCapability)
	}
	return hb
}

func newAttachFixture(t *testing.T, tune ...func(*attachhub.Config)) *attachFixture {
	t.Helper()
	f := &attachFixture{}
	f.testController = newController(t, time.Now(), func(c *api.Config) {
		f.work = c.Work
		for _, fn := range tune {
			fn(&c.Attach)
		}
	})
	ctx := t.Context()
	f.alpha, f.alphaD = f.enrollRealWorker("alpha")
	_, err := f.alpha.Heartbeat(ctx, relayHeartbeat(true))
	must(t, err)
	_, err = f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)
	created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{
		Project: "backend", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA, Mode: domain.ModeInteractive,
	})
	must(t, err)
	f.run = *created.Run
	_, err = f.work.Scheduler.Pass(ctx)
	must(t, err)
	page, err := f.alpha.Assignments(ctx, 0, 0, 0)
	must(t, err)
	if len(page.Commands) != 1 {
		t.Fatalf("alpha was sent %+v", page)
	}
	_, err = f.alpha.AcceptLease(ctx, page.Commands[0].Assign.LeaseID, page.Commands[0].Assign.Epoch)
	must(t, err)
	handle, _ := json.Marshal(v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-abc/" + domain.SessionName(f.run.ID, 1)})
	f.sql(`UPDATE runs SET state = 'running', runtime = 'active', runtime_handle = ? WHERE id = ?`, string(handle), f.run.ID)

	f.beta, f.betaD = f.enrollRealWorker("beta")
	_, err = f.beta.Heartbeat(ctx, relayHeartbeat(true))
	must(t, err)
	w, err := f.owner.Worker(ctx, "alpha")
	must(t, err)
	f.alphaID = domain.ID(w.ID)
	f.laptop, _ = f.enroll("laptop")
	info, err := f.laptop.Describe()
	must(t, err)
	f.laptopID = info.IdentityID
	return f
}

func (f *attachFixture) sql(query string, args ...any) {
	f.t.Helper()
	must(f.t, f.db.WithTx(f.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(f.t.Context(), query, args...)
		return err
	}))
}

func (f *attachFixture) hub() *attachhub.Hub { return f.srv.AttachHub() }

type attachOpen struct {
	conn *client.AttachConn
	err  error
}

func (f *attachFixture) openAsync(c *client.Client, mode string) <-chan attachOpen {
	out := make(chan attachOpen, 1)
	go func() {
		conn, err := c.OpenAttach(f.t.Context(), f.run.ID, "", mode)
		if conn != nil {
			f.t.Cleanup(func() { _ = conn.Close() })
		}
		out <- attachOpen{conn, err}
	}()
	return out
}

func (f *attachFixture) pollAsync(w *client.Client) <-chan []v1.AttachRequest {
	out := make(chan []v1.AttachRequest, 1)
	go func() {
		reqs, err := w.AttachRequests(f.t.Context(), 20*time.Second)
		if err != nil && f.t.Context().Err() == nil {
			f.t.Errorf("poll: %v", err)
		}
		out <- reqs
	}()
	return out
}

func (f *attachFixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(attachWait)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(attachWait):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// request has the Worker poll and returns what it was asked, while the
// client's open is still waiting for the Worker's end.
func (f *attachFixture) request(w *client.Client, wid domain.ID, c *client.Client, mode string) (v1.AttachRequest, <-chan attachOpen) {
	f.t.Helper()
	polled := f.pollAsync(w)
	f.waitFor("the Worker's poll", func() bool { return f.hub().Live(wid) })
	opened := f.openAsync(c, mode)
	reqs := recv(f.t, polled, "the attach request")
	if len(reqs) != 1 {
		f.t.Fatalf("requests = %+v", reqs)
	}
	return reqs[0], opened
}

// join opens a session as c in mode and returns both ends.
func (f *attachFixture) join(c *client.Client, mode string) (cc, wc *client.AttachConn, req v1.AttachRequest) {
	f.t.Helper()
	req, opened := f.request(f.alpha, f.alphaID, c, mode)
	wc, err := f.alpha.DialAttach(f.t.Context(), req.SessionID)
	must(f.t, err)
	f.t.Cleanup(func() { _ = wc.Close() })
	res := recv(f.t, opened, "the client's stream")
	must(f.t, res.err)
	return res.conn, wc, req
}

func readFrame(t *testing.T, c *client.AttachConn) v1.Frame {
	t.Helper()
	type res struct {
		f   v1.Frame
		err error
	}
	ch := make(chan res, 1)
	go func() { f, err := c.ReadFrame(); ch <- res{f, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("reading a frame: %v", r.err)
		}
		return r.f
	case <-time.After(attachWait):
		t.Fatal("timed out reading a frame")
		panic("unreachable")
	}
}

// readUntilError reads until the stream's error frame and returns its code,
// then checks that the stream ends.
func readUntilError(t *testing.T, c *client.AttachConn) domain.Code {
	t.Helper()
	for {
		f := readFrame(t, c)
		if f.Type != v1.FrameError {
			continue
		}
		e, err := v1.ParseError(f.Payload)
		must(t, err)
		type res struct{ err error }
		ch := make(chan res, 1)
		go func() { _, err := c.ReadFrame(); ch <- res{err} }()
		if r := recv(t, ch, "the end of the stream"); !client.IsStreamEnd(r.err) {
			t.Fatalf("after the error frame: %v, want the stream to end", r.err)
		}
		return e.Code
	}
}

// readData reads frames until it has n bytes of data, and returns them.
func readData(t *testing.T, c *client.AttachConn, n int) []byte {
	t.Helper()
	var got []byte
	for len(got) < n {
		if f := readFrame(t, c); f.Type == v1.FrameData {
			got = append(got, f.Payload...)
		}
	}
	return got
}

// readPeers reads until a peers frame lists want (by identity) and returns it.
func readPeers(t *testing.T, c *client.AttachConn, want ...string) []v1.AttachPeer {
	t.Helper()
	for {
		f := readFrame(t, c)
		if f.Type != v1.FramePeers {
			continue
		}
		peers, err := v1.ParsePeers(f.Payload)
		must(t, err)
		ids := make([]string, 0, len(peers))
		for _, p := range peers {
			ids = append(ids, p.ID)
		}
		slices.Sort(ids)
		if slices.Equal(ids, slices.Sorted(slices.Values(want))) {
			return peers
		}
	}
}

// tick lets the sessions revalidate: n sessions have their ticker running.
func (f *attachFixture) tick(n int) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), attachWait)
	defer cancel()
	must(f.t, f.clock.BlockUntil(ctx, n))
	f.clock.Advance(31 * time.Second)
}

func (f *attachFixture) noSessions() {
	f.t.Helper()
	f.waitFor("no session open", func() bool { return f.hub().Open() == 0 })
}

func (f *attachFixture) closedEvents(reason string) []v1.Event {
	f.t.Helper()
	var out []v1.Event
	f.waitFor("an AttachClosed event of "+reason, func() bool {
		page, err := f.owner.Events(f.t.Context(), client.EventQuery{Types: []string{string(domain.EventAttachClosed)}})
		must(f.t, err)
		out = out[:0]
		for _, e := range page.Events {
			if e.Data["reason"] == reason {
				out = append(out, e)
			}
		}
		return len(out) > 0
	})
	return out
}

func TestScenario_LiveAttach_CitaDeUnSoloUso(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()

	t.Run("a Worker identity opening an attach is denied", func(t *testing.T) {
		hc := f.httpClientFor(f.alphaD)
		status, env, _ := doRaw(t, hc, "https://"+f.addr+"/v1/runs/"+f.run.ID+"/attach/stream", http.MethodPost, `{"mode":"view"}`, func(r *http.Request) {
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", v1.AttachUpgrade)
		})
		if status != http.StatusForbidden || env.Code != domain.CodePolicyDenied {
			t.Errorf("a Worker opening: %d %s", status, env.Code)
		}
		f.noSessions()
	})

	t.Run("a second claim is rejected, before and after the session ends", func(t *testing.T) {
		cc, wc, req := f.join(f.owner, v1.AttachModeWrite)
		if req.RunID != f.run.ID || req.LeaseEpoch != 1 || req.Mode != v1.AttachModeWrite || req.Handle.Runtime != "tmux" || req.Client.Name != "owner" {
			t.Errorf("request = %+v", req)
		}
		_, err := f.alpha.DialAttach(ctx, req.SessionID)
		wantCode(t, err, domain.CodeNotFound)
		if n := f.hub().Sessions(); n != 1 {
			t.Errorf("sessions = %d after a replay, want the one", n)
		}
		must(t, cc.Detach())
		f.noSessions()
		_, err = f.alpha.DialAttach(ctx, req.SessionID)
		wantCode(t, err, domain.CodeNotFound)
		_ = wc.Close()
	})

	t.Run("an expired request is rejected and nothing opens", func(t *testing.T) {
		req, opened := f.request(f.alpha, f.alphaID, f.owner, v1.AttachModeView)
		f.clock.Advance(11 * time.Second)
		_, err := f.alpha.DialAttach(ctx, req.SessionID)
		wantCode(t, err, domain.CodeNotFound)
		res := recv(t, opened, "the client's answer")
		wantCode(t, res.err, domain.CodeWorkerOffline)
		f.noSessions()
	})

	t.Run("another Worker cannot claim it, and its own still can", func(t *testing.T) {
		req, opened := f.request(f.alpha, f.alphaID, f.owner, v1.AttachModeView)
		_, err := f.beta.DialAttach(ctx, req.SessionID)
		wantCode(t, err, domain.CodePolicyDenied)
		if f.hub().Sessions() != 0 {
			t.Error("a session was spliced for another Worker")
		}
		wc, err := f.alpha.DialAttach(ctx, req.SessionID)
		must(t, err)
		defer wc.Close()
		res := recv(t, opened, "the client's stream")
		must(t, res.err)
		must(t, res.conn.Detach())
		f.noSessions()
	})

	t.Run("a lease epoch that moved rejects the claim", func(t *testing.T) {
		req, opened := f.request(f.alpha, f.alphaID, f.owner, v1.AttachModeWrite)
		f.sql(`UPDATE runs SET lease_epoch = 2 WHERE id = ?`, f.run.ID)
		defer f.sql(`UPDATE runs SET lease_epoch = 1 WHERE id = ?`, f.run.ID)
		_, err := f.alpha.DialAttach(ctx, req.SessionID)
		wantCode(t, err, domain.CodeStaleLease)
		res := recv(t, opened, "the client's answer")
		wantCode(t, res.err, domain.CodeStaleLease)
		f.noSessions()
	})

	t.Run("a Worker that does not announce the relay gets no requests", func(t *testing.T) {
		gamma, _ := f.enrollRealWorker("gamma")
		_, err := gamma.Heartbeat(ctx, relayHeartbeat(false))
		must(t, err)
		_, err = gamma.AttachRequests(ctx, time.Second)
		wantCode(t, err, domain.CodePolicyDenied)
	})
}

func TestAttachOpen_NoLivePollIsWorkerOffline(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()
	open := func() error {
		conn, err := f.owner.OpenAttach(ctx, f.run.ID, "", v1.AttachModeView)
		if conn != nil {
			_ = conn.Close()
		}
		return err
	}
	// No Worker is polling: the Run is attachable but nobody would answer.
	wantCode(t, open(), domain.CodeWorkerOffline)
	// A Worker whose poll ended a while ago is not live either.
	pollCtx, stop := context.WithCancel(ctx)
	ended := make(chan struct{})
	go func() { defer close(ended); _, _ = f.alpha.AttachRequests(pollCtx, 20*time.Second) }()
	f.waitFor("the poll", func() bool { return f.hub().Live(f.alphaID) })
	stop()
	recv(t, ended, "the poll to end")
	f.waitFor("the Controller to see the poll end", func() bool {
		f.clock.Advance(5 * time.Second) // the end of the poll starts the grace at the time it is seen
		return !f.hub().Live(f.alphaID)
	})
	wantCode(t, open(), domain.CodeWorkerOffline)
}

func TestAttachOpen_AControllerShuttingDownRefuses(t *testing.T) {
	f := newAttachFixture(t)
	f.hub().Shutdown()
	conn, err := f.owner.OpenAttach(t.Context(), f.run.ID, "", v1.AttachModeView)
	if conn != nil {
		_ = conn.Close()
	}
	wantCode(t, err, domain.CodeControllerUnreachable)
}

func TestAttachOpen_ModesAndEligibility(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()
	wantCode2 := func(mode, ref string, code domain.Code) {
		t.Helper()
		conn, err := f.owner.OpenAttach(ctx, ref, "", mode)
		if conn != nil {
			_ = conn.Close()
		}
		wantCode(t, err, code)
	}
	wantCode2(v1.AttachModeShell, f.run.ID, domain.CodePolicyDenied) // the Project has no shell
	wantCode2("root", f.run.ID, domain.CodeInvalidRequest)
	wantCode2("", f.run.ID, domain.CodeInvalidRequest)
	wantCode2(v1.AttachModeView, "run_01JA0000000000000000000000", domain.CodeNotFound)

	// A Run on a Worker that announces no relay is not attachable through it.
	f.sql(`UPDATE runs SET runtime = 'stopped' WHERE id = ?`, f.run.ID)
	wantCode2(v1.AttachModeView, f.run.ID, domain.CodeRunNotAttachable)
	f.sql(`UPDATE runs SET runtime = 'active' WHERE id = ?`, f.run.ID)
	hb := relayHeartbeat(false)
	hb.SSHTarget, hb.AttachBinaries = "alex@box:22", map[string]string{"tmux": "/opt/homebrew/bin/tmux"}
	if _, err := f.alpha.Heartbeat(ctx, hb); err != nil {
		t.Fatal(err)
	}
	conn, err := f.owner.OpenAttach(ctx, f.run.ID, "", v1.AttachModeView)
	if conn != nil {
		_ = conn.Close()
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeRunNotAttachable || de.Details["reason"] != v1.NotAttachableRelayUnsupported {
		t.Fatalf("no relay: %v", err)
	}

	// The route is an upgrade.
	status, env, _ := f.call(f.httpClientFor(f.ownerDir), http.MethodPost, "/v1/runs/"+f.run.ID+"/attach/stream", `{"mode":"view"}`)
	if status != http.StatusBadRequest || env.Code != domain.CodeInvalidRequest {
		t.Errorf("a plain request: %d %s", status, env.Code)
	}
}

func TestScenario_LiveAttach_StreamQueDejaDeValer(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()

	t.Run("a revoked identity", func(t *testing.T) {
		cc, wc, _ := f.join(f.laptop, v1.AttachModeWrite)
		must(t, f.owner.RemoveClient(ctx, f.laptopID))
		f.tick(1)
		if code := readUntilError(t, cc); code != domain.CodeUnauthenticated {
			t.Errorf("client end: %s", code)
		}
		if code := readUntilError(t, wc); code != domain.CodeUnauthenticated {
			t.Errorf("Worker end: %s", code)
		}
		ev := f.closedEvents(v1.AttachCloseRevoked)
		if ev[0].Data["client_id"] != f.laptopID || ev[0].Data["mode"] != v1.AttachModeWrite || ev[0].RunID != f.run.ID ||
			ev[0].WorkerID != string(f.alphaID) || ev[0].Data["code"] != string(domain.CodeUnauthenticated) || ev[0].Actor.ID != f.laptopID {
			t.Errorf("AttachClosed = %+v", ev[0])
		}
		if _, ok := ev[0].Data["duration_ms"]; !ok {
			t.Error("AttachClosed has no duration")
		}
		f.noSessions()
	})

	t.Run("a lease that changed epoch", func(t *testing.T) {
		cc, wc, _ := f.join(f.owner, v1.AttachModeWrite)
		f.sql(`UPDATE runs SET lease_epoch = 2 WHERE id = ?`, f.run.ID)
		f.tick(1)
		if code := readUntilError(t, cc); code != domain.CodeStaleLease {
			t.Errorf("client end: %s", code)
		}
		if code := readUntilError(t, wc); code != domain.CodeStaleLease {
			t.Errorf("Worker end: %s", code)
		}
		f.closedEvents(v1.AttachCloseLease)
		f.noSessions()
	})

	opened := func() []v1.Event {
		page, err := f.owner.Events(ctx, client.EventQuery{Types: []string{string(domain.EventAttachOpened)}})
		must(t, err)
		return page.Events
	}
	if evs := opened(); len(evs) != 2 || evs[0].Data["mode"] != v1.AttachModeWrite {
		t.Errorf("AttachOpened events = %+v", evs)
	}
}

func TestScenario_LiveAttach_ControllerQueSeApaga(t *testing.T) {
	f := newAttachFixture(t)
	cc, wc, _ := f.join(f.owner, v1.AttachModeWrite)
	f.waitFor("the session to be spliced", func() bool { return f.hub().Sessions() == 1 })
	f.stop()
	// Serve has returned, and the daemon closes the store next: the close of the
	// session must be audited by now, with no waiting.
	var closed int
	must(t, f.db.Read(t.Context(), func(q store.Querier) error {
		return q.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE type = ? AND data LIKE ?`,
			string(domain.EventAttachClosed), `%"reason":"`+v1.AttachCloseShutdown+`"%`).Scan(&closed)
	}))
	if closed != 1 {
		t.Errorf("AttachClosed events of a Controller shutdown = %d, want 1", closed)
	}
	if code := readUntilError(t, cc); code != domain.CodeControllerUnreachable {
		t.Errorf("client end: %s", code)
	}
	if code := readUntilError(t, wc); code != domain.CodeControllerUnreachable {
		t.Errorf("Worker end: %s", code)
	}
	// The Run is the Worker's: it goes on.
	var state string
	must(t, f.db.Read(t.Context(), func(q store.Querier) error {
		return q.QueryRow(t.Context(), `SELECT state FROM runs WHERE id = ?`, f.run.ID).Scan(&state)
	}))
	if state != string(domain.RunRunning) {
		t.Errorf("the Run is %s", state)
	}
	if f.hub().Open() != 0 {
		t.Error("the hub holds sessions after it shut down")
	}
}

func TestScenario_LiveAttach_WorkerQueCae(t *testing.T) {
	f := newAttachFixture(t)
	cc, wc, _ := f.join(f.owner, v1.AttachModeWrite)
	must(t, wc.Close())
	if code := readUntilError(t, cc); code != domain.CodeWorkerOffline {
		t.Errorf("client end: %s", code)
	}
	f.closedEvents(v1.AttachCloseWorkerLost)
	// When the Worker is back, the Run can be entered again.
	cc2, wc2, _ := f.join(f.owner, v1.AttachModeView)
	must(t, wc2.WriteData([]byte("hello")))
	if got := readData(t, cc2, 5); string(got) != "hello" {
		t.Errorf("data = %q", got)
	}
}

func TestAttach_ClientDetachTellsTheWorker(t *testing.T) {
	f := newAttachFixture(t)
	cc, wc, _ := f.join(f.owner, v1.AttachModeWrite)
	must(t, cc.WriteData([]byte("ls\r")))
	must(t, cc.Resize(100, 30))
	must(t, cc.Detach())
	if got := readData(t, wc, 3); string(got) != "ls\r" {
		t.Errorf("Worker got %q", got)
	}
	var sawResize, sawClose bool
	for !sawClose {
		switch fr := readFrame(t, wc); fr.Type {
		case v1.FrameResize:
			c, r, err := v1.ParseResize(fr.Payload)
			sawResize = err == nil && c == 100 && r == 30
		case v1.FrameClose:
			sawClose = true
		}
	}
	if !sawResize {
		t.Error("the resize did not reach the Worker")
	}
	f.closedEvents(v1.AttachCloseClient)
	if evs := f.closedEvents(v1.AttachCloseClient); evs[0].Data["code"] != nil {
		t.Errorf("a clean detach carries a code: %+v", evs[0].Data)
	}
}

func TestAttach_AViewSeesTheRunAndSendsNothingToIt(t *testing.T) {
	f := newAttachFixture(t)
	ownerC, ownerW, _ := f.join(f.owner, v1.AttachModeWrite)
	laptopC, laptopW, _ := f.join(f.laptop, v1.AttachModeView)

	// Both are told who is in, and both receive what the Worker end shows.
	ownerID, err := f.owner.Describe()
	must(t, err)
	for name, c := range map[string]*client.AttachConn{"owner": ownerC, "laptop": laptopC} {
		peers := readPeers(t, c, ownerID.IdentityID, f.laptopID)
		for _, p := range peers {
			wantMode := map[string]string{ownerID.IdentityID: v1.AttachModeWrite, f.laptopID: v1.AttachModeView}[p.ID]
			wantName := map[string]string{ownerID.IdentityID: "owner", f.laptopID: "laptop"}[p.ID]
			if p.Mode != wantMode || p.Name != wantName {
				t.Errorf("%s sees peer %+v", name, p)
			}
		}
	}
	must(t, ownerW.WriteData([]byte("one")))
	must(t, laptopW.WriteData([]byte("two")))
	if got := readData(t, ownerC, 3); string(got) != "one" {
		t.Errorf("owner saw %q", got)
	}
	if got := readData(t, laptopC, 3); string(got) != "two" {
		t.Errorf("laptop saw %q", got)
	}

	// What the view sends never reaches the Worker; what the writer sends does.
	must(t, laptopC.WriteData([]byte("from the view")))
	must(t, laptopC.Resize(10, 10))
	must(t, ownerC.WriteData([]byte("from the writer")))
	if got := readData(t, ownerW, len("from the writer")); string(got) != "from the writer" {
		t.Errorf("the writer's Worker end got %q", got)
	}
	must(t, laptopC.Detach())
	for {
		fr := readFrame(t, laptopW)
		if fr.Type == v1.FrameClose {
			break
		}
		t.Fatalf("the view's Worker end got %+v before the close", fr)
	}
	// The one left is told it is alone.
	readPeers(t, ownerC, ownerID.IdentityID)
}

func TestAttach_AWorkerCannotSpeakForTheController(t *testing.T) {
	f := newAttachFixture(t)
	cc, wc, _ := f.join(f.owner, v1.AttachModeWrite)
	forged, err := v1.PeersFrame([]v1.AttachPeer{{ID: "client_forged", Name: "root", Mode: v1.AttachModeWrite}})
	must(t, err)
	must(t, wc.WriteFrame(forged))
	must(t, wc.WriteData([]byte("after")))
	for {
		fr := readFrame(t, cc)
		if fr.Type == v1.FrameData {
			break
		}
		if fr.Type == v1.FramePeers {
			peers, _ := v1.ParsePeers(fr.Payload)
			for _, p := range peers {
				if p.ID == "client_forged" {
					t.Fatalf("a peers frame of the Worker reached the client: %+v", peers)
				}
			}
		}
	}
}

func TestAttach_ASlowClientIsCutWithoutHoldingUpTheOther(t *testing.T) {
	f := newAttachFixture(t)
	ownerC, ownerW, _ := f.join(f.owner, v1.AttachModeWrite)
	_, laptopW, _ := f.join(f.laptop, v1.AttachModeWrite) // never reads

	chunk := bytes.Repeat([]byte{'x'}, v1.MaxFramePayload/2)
	const fast = 16 // 512 KiB: it fits the buffer whatever the client does
	cut := make(chan error, 1)
	go func() {
		for range 4000 {
			if err := laptopW.WriteData(chunk); err != nil {
				cut <- nil
				return
			}
		}
		cut <- errors.New("the slow client was never cut")
	}()
	fastDone := make(chan error, 1)
	go func() {
		for range fast {
			if err := ownerW.WriteData(chunk); err != nil {
				fastDone <- err
				return
			}
		}
		fastDone <- nil
	}()
	got := make(chan int, 1)
	go func() {
		n := 0
		for n < fast*len(chunk) {
			fr, err := ownerC.ReadFrame()
			if err != nil {
				break
			}
			if fr.Type == v1.FrameData {
				n += len(fr.Payload)
			}
		}
		got <- n
	}()
	if err := recv(t, fastDone, "the fast Worker end"); err != nil {
		t.Fatalf("the fast side was held up: %v", err)
	}
	if n := recv(t, got, "the fast client"); n != fast*len(chunk) {
		t.Errorf("the fast client got %d bytes, want %d", n, fast*len(chunk))
	}
	if err := recv(t, cut, "the slow client to be cut"); err != nil {
		t.Fatal(err)
	}
	f.closedEvents(v1.AttachCloseSlowClient)
	// The other session is still up.
	if f.hub().Sessions() != 1 {
		t.Errorf("sessions = %d, want 1", f.hub().Sessions())
	}
}

// What a peer sent in the same write as its request is not lost to the hijack.
func TestAttach_BytesBufferedBeforeTheUpgradeAreKept(t *testing.T) {
	f := newAttachFixture(t)
	creds, err := client.LoadCredentials(f.ownerDir)
	must(t, err)
	pair, err := tls.X509KeyPair(creds.CertPEM, creds.KeyPEM)
	must(t, err)
	roots := f.httpClient().Transport.(*http.Transport).TLSClientConfig.RootCAs

	req, opened := f.request(f.alpha, f.alphaID, f.owner, v1.AttachModeWrite)
	// Let the first request finish as it would, then do the raw one.
	wc, err := f.alpha.DialAttach(t.Context(), req.SessionID)
	must(t, err)
	defer wc.Close()
	recv(t, opened, "the first stream")

	polled := f.pollAsync(f.alpha)
	f.waitFor("the poll", func() bool { return f.hub().Live(f.alphaID) })
	conn, err := (&tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}, ServerName: "127.0.0.1"}}).DialContext(t.Context(), "tcp", f.addr)
	must(t, err)
	defer conn.Close()
	body := `{"mode":"write"}`
	frame, _ := v1.AppendFrame(nil, v1.DataFrame([]byte("early bird")))
	_, err = fmt.Fprintf(conn, "POST /v1/runs/%s/attach/stream HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: %s\r\nContent-Length: %d\r\n\r\n%s%s",
		f.run.ID, v1.AttachUpgrade, len(body), body, frame)
	must(t, err)
	reqs := recv(t, polled, "the second request")
	wc2, err := f.alpha.DialAttach(t.Context(), reqs[0].SessionID)
	must(t, err)
	defer wc2.Close()
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := readData(t, wc2, len("early bird")); string(got) != "early bird" {
		t.Errorf("the Worker got %q", got)
	}
	// And the other way: the stream answers on the same connection.
	must(t, wc2.WriteData([]byte("pong")))
	for {
		fr, err := v1.ReadFrame(br)
		must(t, err)
		if fr.Type == v1.FrameData {
			if string(fr.Payload) != "pong" {
				t.Errorf("the client got %q", fr.Payload)
			}
			break
		}
	}
}

// A screen stream is granted at a clamped rate, is not audited as an entry or an
// exit.
func TestAttach_AScreenIsClampedAndUnaudited(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()
	writer, _, _ := f.join(f.owner, v1.AttachModeWrite)
	ownerID, err := f.owner.Describe()
	must(t, err)
	readPeers(t, writer, ownerID.IdentityID)

	// The rate the client asks for reaches the Worker clamped; none asks for the default.
	for _, c := range []struct{ asked, want int }{{0, v1.ScreenDefaultHz}, {1, 1}, {99, v1.ScreenMaxHz}, {-3, v1.ScreenMinHz}} {
		polled := f.pollAsync(f.alpha)
		f.waitFor("the Worker's poll", func() bool { return f.hub().Live(f.alphaID) })
		opened := make(chan *client.ScreenStream, 1)
		go func() {
			s, err := f.laptop.OpenScreen(ctx, f.run.ID, "", c.asked)
			must(t, err)
			opened <- s
		}()
		reqs := recv(t, polled, "the screen request")
		if len(reqs) != 1 || reqs[0].Mode != v1.AttachModeScreen || reqs[0].Hz != c.want {
			t.Fatalf("asked %d: request = %+v, want a screen at %d Hz", c.asked, reqs, c.want)
		}
		wc, err := f.alpha.DialAttach(ctx, reqs[0].SessionID)
		must(t, err)
		t.Cleanup(func() { _ = wc.Close() })
		f.waitFor("the screen to be spliced", func() bool { return f.hub().Sessions() == 2 })
		s := recv(t, opened, "the screen stream")
		must(t, wc.WriteData([]byte("a screen")))
		if got, err := s.Next(); err != nil || string(got) != "a screen" {
			t.Fatalf("screen = %q, %v", got, err)
		}
		_ = s.Close()
		f.waitFor("the screen to end", func() bool { return f.hub().Sessions() == 1 })
	}

	// Nothing of the screens is in the audit.
	if evs, err := f.owner.Events(ctx, client.EventQuery{Types: []string{string(domain.EventAttachOpened), string(domain.EventAttachClosed)}}); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range evs.Events {
			if e.Data["mode"] == v1.AttachModeScreen {
				t.Errorf("a screen stream was audited: %+v", e)
			}
		}
	}
	must(t, writer.Detach())
}

// A panel that only watches is not "who else is inside": a writer's peers list
// never has it, however many come and go.
func TestAttach_AScreenIsNotAPeer(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()
	writer, _, _ := f.join(f.owner, v1.AttachModeWrite)
	ownerID, err := f.owner.Describe()
	must(t, err)
	readPeers(t, writer, ownerID.IdentityID)

	polled := f.pollAsync(f.alpha)
	f.waitFor("the Worker's poll", func() bool { return f.hub().Live(f.alphaID) })
	opened := make(chan *client.ScreenStream, 1)
	go func() {
		s, err := f.laptop.OpenScreen(ctx, f.run.ID, "", 0)
		must(t, err)
		opened <- s
	}()
	reqs := recv(t, polled, "the screen request")
	wc, err := f.alpha.DialAttach(ctx, reqs[0].SessionID)
	must(t, err)
	t.Cleanup(func() { _ = wc.Close() })
	f.waitFor("the screen to be spliced", func() bool { return f.hub().Sessions() == 2 })
	s := recv(t, opened, "the screen stream")
	t.Cleanup(func() { _ = s.Close() })

	// Someone else joins while the panel is there: the list the writer gets has
	// the two of them and not the panel (readPeers waits for exactly this list).
	_, _, _ = f.join(f.owner, v1.AttachModeView)
	readPeers(t, writer, ownerID.IdentityID, ownerID.IdentityID)

	// The panel is told who is inside, though it is not one of them: the TUI
	// shows it in its title. It is told when it joins and whenever the list changes.
	deadline := time.Now().Add(attachWait)
	for {
		must(t, wc.WriteData([]byte("screen")))
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		if peers := s.Peers(); len(peers) == 2 {
			for _, p := range peers {
				if p.Mode == v1.AttachModeScreen {
					t.Fatalf("the panel is listed among the peers: %+v", peers)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panel never learned who is inside: %+v", s.Peers())
		}
	}
}
