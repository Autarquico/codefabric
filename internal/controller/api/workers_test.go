package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func heartbeat(epoch string) v1.HeartbeatRequest {
	return v1.HeartbeatRequest{
		Epoch: epoch, DaemonVersion: controllerVersion, Hostname: "box.lan", OS: "linux", Arch: "amd64",
		Total:  v1.Capacity{CPU: 8, MemoryMB: 16384, DiskMB: 100000},
		Health: v1.Health{Status: v1.HealthOK},
		// New Runs are schema 4 unless their Project denies the repo's configuration.
		Capabilities: []string{domain.RepoConfigCapability},
	}
}

func newEpoch() string { return ulid.Make().String() }

func TestScenario_WorkerRegistryHeartbeat_AlcanceDeIdentidades(t *testing.T) {
	tc := newController(t, time.Now())
	worker, workerDir := tc.enrollRealWorker("box")
	workerHTTP := tc.httpClientFor(workerDir)
	ownerHTTP := tc.httpClientFor(tc.ownerDir)
	anonymous := tc.httpClient()
	beat, err := json.Marshal(heartbeat(newEpoch()))
	must(t, err)

	// A Worker's certificate reaches no endpoint of clients: not the work, not
	// the administration of Workers, not the other identities.
	for _, p := range []struct{ method, path string }{
		{"GET", "/v1/tasks"},
		{"POST", "/v1/tasks"},
		{"GET", "/v1/projects"},
		{"GET", "/v1/runs"},
		{"GET", "/v1/workers"},
		{"GET", "/v1/workers/box"},
		{"POST", "/v1/workers/box/drain"},
		{"POST", "/v1/workers/box/undrain"},
		{"DELETE", "/v1/workers/box"},
		{"GET", "/v1/clients"},
		{"POST", "/v1/tokens"},
		{"DELETE", "/v1/clients/client_01JA0000000000000000000000"},
		{"GET", "/v1/status"},
		{"GET", "/v1/worker/anything"},
	} {
		if code, body, _ := tc.call(workerHTTP, p.method, p.path, "{}"); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("worker %s %s: %d %s", p.method, p.path, code, body.Code)
		}
	}
	if code, _, _ := tc.call(workerHTTP, http.MethodPost, "/v1/worker/heartbeat", string(beat)); code != http.StatusOK {
		t.Errorf("worker heartbeat: %d", code)
	}

	// A client's certificate reaches nothing under /v1/worker/, and anonymous callers not even that.
	for _, path := range []string{"/v1/worker/heartbeat", "/v1/worker/anything"} {
		if code, body, _ := tc.call(ownerHTTP, http.MethodPost, path, string(beat)); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("client POST %s: %d %s", path, code, body.Code)
		}
	}
	if code, _, _ := tc.call(ownerHTTP, http.MethodGet, "/v1/tasks", ""); code != http.StatusOK {
		t.Errorf("client GET /v1/tasks: %d", code)
	}
	if code, body, _ := tc.call(anonymous, http.MethodPost, "/v1/worker/heartbeat", string(beat)); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
		t.Errorf("anonymous heartbeat: %d %s", code, body.Code)
	}

	// The exception: any identity renews its own certificate, so a Worker does,
	// and keeps working with the new one.
	must(t, worker.Renew(t.Context()))
	if _, err := worker.Heartbeat(t.Context(), heartbeat(newEpoch())); err != nil {
		t.Errorf("heartbeat after renewing: %v", err)
	}
}

func TestHeartbeat_TheRequestCannotNameAWorker(t *testing.T) {
	tc := newController(t, time.Now())
	_, dir := tc.enrollRealWorker("a")
	tc.enrollRealWorker("b")
	raw := tc.httpClientFor(dir)

	body := `{"worker_id":"worker_01JA0000000000000000000000","epoch":"` + newEpoch() + `","daemon_version":"v0.3.0",` +
		`"hostname":"h","os":"linux","arch":"amd64","total":{"cpu":1,"memory_mb":1,"disk_mb":1},"health":{"status":"ok"}}`
	if code, resp, _ := tc.call(raw, http.MethodPost, "/v1/worker/heartbeat", body); code != http.StatusBadRequest || resp.Code != domain.CodeInvalidRequest {
		t.Errorf("heartbeat with a worker_id: %d %s", code, resp.Code)
	}
	for _, w := range workersOf(t, tc.owner) {
		if w.State != domain.WorkerJoining {
			t.Errorf("%s is %s after a refused heartbeat", w.Name, w.State)
		}
	}
}

func workersOf(t *testing.T, c *client.Client) []v1.Worker {
	t.Helper()
	ws, err := c.Workers(t.Context(), false)
	must(t, err)
	return ws
}

// seedRun puts a Run on the Worker, behind the API's back: assigning Runs is a later milestone.
func (tc *testController) seedRun(worker string, state domain.RunState) {
	tc.t.Helper()
	ctx := tc.t.Context()
	must(tc.t, tc.db.WithTx(ctx, func(tx *store.Tx) error {
		p := store.Project{ID: "project_1", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: time.Now()}
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		task := store.Task{
			ID: "task_1", Short: 1, ProjectID: p.ID, Name: "t", Prompt: "x", State: domain.TaskRunning,
			CreatedBy: domain.CreatorHuman, CreatedAt: time.Now(),
		}
		if err := tx.InsertTask(ctx, task); err != nil {
			return err
		}
		return tx.InsertRun(ctx, store.Run{
			ID: "run_1", Short: 1, TaskID: task.ID, Attempt: 1, Lineage: domain.LineageInitial, Spec: []byte(`{}`),
			SpecDigest: "d", SourceCommit: "c", State: state, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, WorkerID: domain.ID(worker), CreatedAt: time.Now(),
		})
	}))
}

func TestWorkers_AdministrationThroughTheSDK(t *testing.T) {
	tc := newController(t, time.Now())
	worker, dir := tc.enrollRealWorker("box", v1.WorkerSettings{CPUMax: 4})
	id, err := client.Describe(dir)
	must(t, err)
	epoch := newEpoch()
	ctx := t.Context()

	// Joined, not yet heard from.
	got, err := tc.owner.Worker(ctx, "box")
	must(t, err)
	if got.State != domain.WorkerJoining || got.ID != id.IdentityID || got.LastHeartbeatAt != nil || got.Contribution.CPUMax != 4 {
		t.Fatalf("before the first heartbeat: %+v", got)
	}

	resp, err := worker.Heartbeat(ctx, heartbeat(epoch))
	must(t, err)
	if resp.State != domain.WorkerOnline {
		t.Fatalf("heartbeat answered %s", resp.State)
	}
	list, err := tc.owner.Workers(ctx, false)
	must(t, err)
	if len(list) != 1 || list[0].Resources.Allocatable.CPU != 4 || list[0].Resources.Total.CPU != 8 ||
		list[0].DaemonVersion != controllerVersion || !list[0].Compatible {
		t.Fatalf("list = %+v", list)
	}
	for _, ref := range []string{"worker-1", id.IdentityID} {
		if w, err := tc.owner.Worker(ctx, ref); err != nil || w.Name != "box" {
			t.Errorf("show %s: %+v, %v", ref, w, err)
		}
	}
	_, err = tc.owner.Worker(ctx, "nowhere")
	wantCode(t, err, domain.CodeNotFound)

	// Drain, then the heartbeat finishes it; undrain brings it back.
	if w, err := tc.owner.DrainWorker(ctx, "box"); err != nil || w.State != domain.WorkerDraining {
		t.Fatalf("drain: %+v, %v", w, err)
	}
	resp, err = worker.Heartbeat(ctx, heartbeat(epoch))
	must(t, err)
	if resp.State != domain.WorkerDrained {
		t.Fatalf("heartbeat while draining answered %s", resp.State)
	}
	if w, err := tc.owner.UndrainWorker(ctx, "box"); err != nil || w.State != domain.WorkerOnline {
		t.Fatalf("undrain: %+v, %v", w, err)
	}

	// With a Run it cannot be removed (409), and the refusal does not touch its identity.
	tc.seedRun(id.IdentityID, domain.RunRunning)
	_, err = tc.owner.RemoveWorker(ctx, "box")
	wantCode(t, err, domain.CodeWorkerBusy)
	if code, body, _ := tc.call(tc.httpClientFor(tc.ownerDir), http.MethodDelete, "/v1/workers/box", ""); code != http.StatusConflict || body.Code != domain.CodeWorkerBusy {
		t.Errorf("DELETE with a Run: %d %s", code, body.Code)
	}
	if _, err := worker.Heartbeat(ctx, heartbeat(epoch)); err != nil {
		t.Fatalf("the Worker lost access after a refused removal: %v", err)
	}
	must(t, tc.db.WithTx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runs SET state = 'completed', publication = 'pending', finished_at = ?`, time.Now().UnixNano())
		return err
	}))

	// Without it, removal revokes the certificate for good.
	removed, err := tc.owner.RemoveWorker(ctx, "box")
	must(t, err)
	if removed.State != domain.WorkerRemoved {
		t.Fatalf("removed = %+v", removed)
	}
	_, err = worker.Heartbeat(ctx, heartbeat(epoch))
	wantCode(t, err, domain.CodeUnauthenticated)
	wantCode(t, worker.Renew(ctx), domain.CodeUnauthenticated)
	if list := workersOf(t, tc.owner); len(list) != 0 {
		t.Errorf("a removed Worker is listed: %+v", list)
	}
	all, err := tc.owner.Workers(ctx, true)
	if err != nil || len(all) != 1 || all[0].State != domain.WorkerRemoved {
		t.Errorf("all = %+v, %v", all, err)
	}
}

func TestWorkers_ListIsAnEmptyArrayNotNull(t *testing.T) {
	tc := newController(t, time.Now())
	_, _, raw := tc.call(tc.httpClientFor(tc.ownerDir), http.MethodGet, "/v1/workers", "")
	if string(raw) != "{\"workers\":[]}\n" {
		t.Errorf("body = %q", raw)
	}
}
