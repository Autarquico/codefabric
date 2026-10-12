package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/domain"
)

// schedulingFixture is a Controller with two online Workers and a queued Run
// that the scheduler has leased to "alpha".
type schedulingFixture struct {
	*testController
	work          *app.Services
	alpha, beta   *client.Client
	alphaD, betaD string
	run           v1.Run
	assign        v1.AssignCommand
}

func newSchedulingFixture(t *testing.T) *schedulingFixture {
	t.Helper()
	f := &schedulingFixture{}
	f.testController = newController(t, time.Now(), func(c *api.Config) { f.work = c.Work })
	ctx := t.Context()
	f.alpha, f.alphaD = f.enrollRealWorker("alpha")
	f.beta, f.betaD = f.enrollRealWorker("beta")
	for _, w := range []*client.Client{f.alpha, f.beta} {
		_, err := w.Heartbeat(ctx, heartbeat(newEpoch()))
		must(t, err)
	}
	_, err := f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)
	created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA})
	must(t, err)
	f.run = *created.Run
	_, err = f.work.Scheduler.Pass(ctx)
	must(t, err)

	page, err := f.alpha.Assignments(ctx, 0, 0, 0)
	must(t, err)
	if len(page.Commands) != 1 {
		t.Fatalf("alpha was sent %+v", page)
	}
	f.assign = *page.Commands[0].Assign
	return f
}

func TestAssignmentsFlowOverHTTP(t *testing.T) {
	f := newSchedulingFixture(t)
	ctx := t.Context()
	if f.assign.RunID != f.run.ID || f.assign.Epoch != 1 || f.assign.Resources.CPU != 2 {
		t.Fatalf("assign = %+v", f.assign)
	}
	if page, err := f.beta.Assignments(ctx, 0, 0, 0); err != nil || len(page.Commands) != 0 {
		t.Errorf("beta was sent %+v (%v)", page, err)
	}

	lease, err := f.alpha.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
	must(t, err)
	if lease.State != domain.LeaseAccepted {
		t.Fatalf("lease = %+v", lease)
	}
	// The same accept again: the same bytes.
	path := "/v1/worker/leases/" + f.assign.LeaseID + "/accept"
	hc := f.httpClientFor(f.alphaD)
	_, _, first := f.call(hc, http.MethodPost, path, `{"epoch":1}`)
	_, _, second := f.call(hc, http.MethodPost, path, `{"epoch":1}`)
	if string(first) != string(second) || !strings.Contains(string(first), `"accepted"`) {
		t.Errorf("answers differ:\n%s\n%s", first, second)
	}

	acked, err := f.alpha.AckCommands(ctx, 1)
	must(t, err)
	if acked.AckedThrough != 1 {
		t.Errorf("ack = %+v", acked)
	}
	run, err := f.owner.Run(ctx, f.run.ID)
	must(t, err)
	if run.State != domain.RunPreparing || run.Lease == nil || run.Lease.State != domain.LeaseAccepted || run.WorkerID == "" {
		t.Errorf("Run = %+v", run)
	}
	expl, err := f.owner.ExplainRun(ctx, f.run.ID)
	must(t, err)
	if expl.Decision != v1.DecisionAssigned || expl.Chosen != "alpha" {
		t.Errorf("explanation = %+v", expl)
	}
}

func TestLeaseErrorsOverHTTP(t *testing.T) {
	f := newSchedulingFixture(t)
	ctx := t.Context()
	alphaHTTP, betaHTTP := f.httpClientFor(f.alphaD), f.httpClientFor(f.betaD)
	accept := "/v1/worker/leases/" + f.assign.LeaseID + "/accept"
	reject := "/v1/worker/leases/" + f.assign.LeaseID + "/reject"

	// Another Worker's lease is as unknown as none.
	for path, body := range map[string]string{accept: `{"epoch":1}`, reject: `{"epoch":1,"code":"CF_WORKER_CAPACITY"}`} {
		if code, got, _ := f.call(betaHTTP, http.MethodPost, path, body); code != http.StatusNotFound || got.Code != domain.CodeNotFound {
			t.Errorf("beta %s: %d %s", path, code, got.Code)
		}
	}
	// Beta cannot acknowledge alpha's orders: its own queue is empty.
	if code, body, _ := f.call(betaHTTP, http.MethodPost, "/v1/worker/commands/ack", `{"up_to_seq":1}`); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("beta ack: %d %s", code, body.Code)
	}
	// The wrong epoch is stale: 409.
	if code, body, _ := f.call(alphaHTTP, http.MethodPost, accept, `{"epoch":9}`); code != http.StatusConflict || body.Code != domain.CodeStaleLease {
		t.Errorf("wrong epoch: %d %s", code, body.Code)
	}
	if code, body, _ := f.call(alphaHTTP, http.MethodPost, reject, `{"epoch":1,"code":"CF_STALE_LEASE"}`); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("bad reject code: %d %s", code, body.Code)
	}
	if code, body, _ := f.call(alphaHTTP, http.MethodPost, accept, `{"epoch":1,"extra":true}`); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("unknown field: %d %s", code, body.Code)
	}
	for _, q := range []string{"after_seq=-1", "wait=x", "limit=1.5"} {
		if code, body, _ := f.call(alphaHTTP, http.MethodGet, "/v1/worker/assignments?"+q, ""); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, code, body.Code)
		}
	}

	// An expired lease: 409 CF_STALE_LEASE.
	f.clock.Advance(time.Hour)
	_, err := f.work.Leases.SweepExpired(ctx)
	must(t, err)
	_, err = f.alpha.AcceptLease(ctx, f.assign.LeaseID, 1)
	wantCode(t, err, domain.CodeStaleLease)
	if code, _, _ := f.call(alphaHTTP, http.MethodPost, accept, `{"epoch":1}`); code != http.StatusConflict {
		t.Errorf("accept of an expired lease: %d", code)
	}
}

func TestSchedulingRoutesAreScoped(t *testing.T) {
	f := newSchedulingFixture(t)
	ownerHTTP, alphaHTTP, anonymous := f.httpClientFor(f.ownerDir), f.httpClientFor(f.alphaD), f.httpClient()

	for _, p := range []struct{ method, path string }{
		{"GET", "/v1/worker/assignments"},
		{"POST", "/v1/worker/leases/" + f.assign.LeaseID + "/accept"},
		{"POST", "/v1/worker/leases/" + f.assign.LeaseID + "/reject"},
		{"POST", "/v1/worker/commands/ack"},
	} {
		if code, body, _ := f.call(ownerHTTP, p.method, p.path, "{}"); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("client %s %s: %d %s", p.method, p.path, code, body.Code)
		}
		if code, body, _ := f.call(anonymous, p.method, p.path, "{}"); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
			t.Errorf("anonymous %s %s: %d %s", p.method, p.path, code, body.Code)
		}
	}
	explain := "/v1/runs/" + f.run.ID + "/explain"
	if code, body, _ := f.call(alphaHTTP, http.MethodGet, explain, ""); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
		t.Errorf("worker explain: %d %s", code, body.Code)
	}
	if code, _, _ := f.call(ownerHTTP, http.MethodGet, explain, ""); code != http.StatusOK {
		t.Errorf("client explain: %d", code)
	}
	if code, body, _ := f.call(ownerHTTP, http.MethodGet, "/v1/runs/run-99/explain", ""); code != http.StatusNotFound || body.Code != domain.CodeNotFound {
		t.Errorf("explain of an unknown Run: %d %s", code, body.Code)
	}
}

var conflictCodes = []domain.Code{
	domain.CodeStaleLease, domain.CodeWorkerCapacity, domain.CodeNoEligibleWorker,
	domain.CodeHarnessUnavailable, domain.CodeVersionIncompatible,
}

func TestSchedulerCodesTravelAs409(t *testing.T) {
	tc := newControllerWithRoutes(t, time.Now(), func(s *api.Server) {
		for _, code := range conflictCodes {
			s.AddRoute("GET /v1/test/"+string(code), []domain.Kind{domain.KindClient}, func(http.ResponseWriter, *http.Request) error {
				return domain.NewError(code, "x", nil)
			})
		}
	})
	hc := tc.httpClientFor(tc.ownerDir)
	for _, code := range conflictCodes {
		if status, body, _ := tc.call(hc, http.MethodGet, "/v1/test/"+string(code), ""); status != http.StatusConflict || body.Code != code {
			t.Errorf("%s: %d %s", code, status, body.Code)
		}
	}
}

func TestAssignmentsLongPollOverHTTP(t *testing.T) {
	f := newSchedulingFixture(t)
	ctx := t.Context()
	// A poll with nothing for it waits for the commit that brings the order.
	got := make(chan v1.AssignmentsResponse, 1)
	go func() {
		page, err := f.beta.Assignments(ctx, 0, 20*time.Second, 0)
		if err != nil {
			t.Error(err)
		}
		got <- page
	}()
	time.Sleep(100 * time.Millisecond)
	_, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Prompt: "second", ResolvedRef: "main", SourceCommit: commitA})
	must(t, err)
	_, err = f.work.Scheduler.Pass(ctx)
	must(t, err)
	select {
	case page := <-got:
		// Alpha holds a reservation already, so the second Run goes to beta.
		if len(page.Commands) != 1 || page.Commands[0].Seq != 1 || page.LastSeq != 1 {
			t.Errorf("page = %+v", page)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long-poll did not return when its order was committed")
	}
}

// A Worker that hangs up in the middle of a long-poll is not an error of the Controller.
func TestAssignmentsHangUpIsNotAnError(t *testing.T) {
	f := newSchedulingFixture(t)
	_, err := f.beta.Assignments(t.Context(), 0, 0, 0) // beta has nothing: its poll waits
	must(t, err)
	ctx, hangUp := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := f.beta.Assignments(ctx, 0, 20*time.Second, 0)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond) // the request is on the server, waiting
	hangUp()
	if err := <-done; err == nil {
		t.Fatal("the cancelled poll succeeded")
	}
	// The fixture polled once and the test once more: the third is the hung-up request, logged when its handler returns.
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(f.logs.String(), "path=/v1/worker/assignments") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the hung-up request was never logged:\n%s", f.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	logs := f.logs.String()
	if strings.Contains(logs, "level=ERROR") || strings.Contains(logs, "status=500") || strings.Contains(logs, "request failed") {
		t.Errorf("a hang-up was logged as a failure:\n%s", logs)
	}
}

func TestRunReportOverHTTP(t *testing.T) {
	f := newSchedulingFixture(t)
	ctx := t.Context()
	_, err := f.alpha.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
	must(t, err)
	at := time.Now().UTC() // an entry sent again says the same thing, time included
	entry := func(seq uint64, kind v1.RunReportKind) v1.RunReportEntry {
		en := v1.RunReportEntry{WorkerEpoch: "01J00000000000000000000000", WorkerSeq: seq, LeaseEpoch: 1, Kind: kind, At: at}
		if kind == v1.ReportStarted {
			en.Handle = &v1.RuntimeHandle{Runtime: "tmux", Ref: "s/n"}
		}
		return en
	}

	got, err := f.alpha.ReportRun(ctx, f.run.ID, v1.RunReportRequest{Entries: []v1.RunReportEntry{entry(1, v1.ReportStarted)}})
	must(t, err)
	if len(got.Outcomes) != 1 || got.Outcomes[0].Result != v1.ReportApplied || got.Run.State != domain.RunRunning || got.Run.Runtime != domain.RuntimeActive {
		t.Errorf("report = %+v", got)
	}
	// Sent again: a duplicate.
	got, err = f.alpha.ReportRun(ctx, f.run.ID, v1.RunReportRequest{Entries: []v1.RunReportEntry{entry(1, v1.ReportStarted)}})
	must(t, err)
	if got.Outcomes[0].Result != v1.ReportDuplicate {
		t.Errorf("replay = %+v", got.Outcomes)
	}

	// A Worker that was not offered the Run, and a client, have no access.
	_, err = f.beta.ReportRun(ctx, f.run.ID, v1.RunReportRequest{Entries: []v1.RunReportEntry{entry(1, v1.ReportStarted)}})
	wantCode(t, err, domain.CodeNotFound)
	path := "/v1/worker/runs/" + f.run.ID + "/report"
	if code, body, _ := f.call(f.httpClientFor(f.ownerDir), http.MethodPost, path, "{}"); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
		t.Errorf("client: %d %s", code, body.Code)
	}
	if code, body, _ := f.call(f.httpClient(), http.MethodPost, path, "{}"); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
		t.Errorf("anonymous: %d %s", code, body.Code)
	}
	alphaHTTP := f.httpClientFor(f.alphaD)
	if code, body, _ := f.call(alphaHTTP, http.MethodPost, path, `{"entries":[],"extra":1}`); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("unknown field: %d %s", code, body.Code)
	}
	if code, body, _ := f.call(alphaHTTP, http.MethodPost, path, `{"entries":[]}`); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("no entries: %d %s", code, body.Code)
	}
}
