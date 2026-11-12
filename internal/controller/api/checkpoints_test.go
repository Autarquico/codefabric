package api_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// tipRelay is a relay that knows where refs point to: what the Worker pushed.
type tipRelay struct {
	mu   sync.Mutex
	tips map[string]string
}

func (r *tipRelay) BranchTip(context.Context, domain.ID, string) (string, bool, error) {
	return "", false, nil
}

func (r *tipRelay) RefTip(_ context.Context, project domain.ID, ref string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oid, ok := r.tips[string(project)+"/"+ref]
	return oid, ok, nil
}

func (r *tipRelay) push(project, ref, oid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tips[project+"/"+ref] = oid
}

// checkpointFixture is a Controller with two Workers able to capture, each
// with a started Run of its own.
type checkpointFixture struct {
	*testController
	relay         *tipRelay
	alpha, beta   *client.Client
	alphaD, betaD string
	mine, theirs  v1.Run // the Runs of alpha and of beta
	mineEpoch     uint64
	theirsEpoch   uint64
	project       string
}

func newCheckpointFixture(t *testing.T) *checkpointFixture {
	t.Helper()
	f := &checkpointFixture{relay: &tipRelay{tips: map[string]string{}}, project: "backend"}
	var work *app.Services
	f.testController = newController(t, time.Now(), func(c *api.Config) {
		bs, err := blob.New(t.TempDir(), c.Clock)
		must(t, err)
		c.Work = app.New(c.Store.(*store.DB), c.Clock, domain.NewIDGenerator(c.Clock.Now, rand.Reader),
			app.Config{Workers: workers.Config{ControllerVersion: controllerVersion}, Blobs: bs, Relay: f.relay, CheckpointExclude: []string{"*.pem"}},
			runs.WithRelay(f.relay))
		work = c.Work
	})
	ctx := t.Context()
	f.alpha, f.alphaD = f.enrollRealWorker("alpha")
	f.beta, f.betaD = f.enrollRealWorker("beta")
	for _, w := range []*client.Client{f.alpha, f.beta} {
		beat := heartbeat(newEpoch())
		beat.Capabilities = append(beat.Capabilities, domain.CheckpointCapability)
		_, err := w.Heartbeat(ctx, beat)
		must(t, err)
	}
	_, err := f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: f.project, Repo: "/srv/r"})
	must(t, err)
	keep := true
	_, err = f.owner.UpdateProject(ctx, f.project, v1.UpdateProjectRequest{NativeState: &keep})
	must(t, err)
	for _, name := range []string{"one", "two"} {
		created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{Project: f.project, Name: name, Prompt: "x", ResolvedRef: "main", SourceCommit: commitA})
		must(t, err)
		if name == "one" {
			f.mine = *created.Run
		} else {
			f.theirs = *created.Run
		}
	}
	_, err = work.Scheduler.Pass(ctx)
	must(t, err)
	// The scheduler may give either Worker either Run: alpha and beta are named
	// after the Run they ended up with.
	enrolled := []struct {
		c   *client.Client
		dir string
	}{{f.alpha, f.alphaD}, {f.beta, f.betaD}}
	for _, w := range enrolled {
		page, err := w.c.Assignments(ctx, 0, 0, 0)
		must(t, err)
		if len(page.Commands) != 1 {
			t.Fatalf("a Worker was sent %+v", page)
		}
		a := page.Commands[0].Assign
		_, err = w.c.AcceptLease(ctx, a.LeaseID, a.Epoch)
		must(t, err)
		_, err = w.c.ReportRun(ctx, a.RunID, v1.RunReportRequest{Entries: []v1.RunReportEntry{{
			WorkerEpoch: newEpoch(), WorkerSeq: 1, LeaseEpoch: a.Epoch, Kind: v1.ReportStarted, At: time.Now(),
			Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "s/n"},
		}}})
		must(t, err)
		if a.RunID == f.mine.ID {
			f.alpha, f.alphaD, f.mineEpoch = w.c, w.dir, a.Epoch
		} else {
			f.beta, f.betaD, f.theirsEpoch = w.c, w.dir, a.Epoch
		}
	}
	return f
}

func (f *checkpointFixture) request() v1.CheckpointRequested {
	f.t.Helper()
	out, err := f.owner.RequestCheckpoint(f.t.Context(), f.mine.ID, "")
	must(f.t, err)
	return out
}

func sha(s string) string { return digestOf(s) }

func (f *checkpointFixture) upload(c *client.Client, run string, epoch uint64, cp, content string) error {
	_, err := c.PutNativeState(f.t.Context(), run, cp, epoch, sha(content), strings.NewReader(content), int64(len(content)))
	return err
}

func (f *checkpointFixture) complete(cp, commit string, native *v1.NativeStateRef) (v1.CheckpointReported, error) {
	f.relay.push(string(f.projectID()), domain.CheckpointRef(domain.ID(cp)), commit)
	return f.alpha.CompleteCheckpoint(f.t.Context(), f.mine.ID, cp, v1.CheckpointCompleteRequest{
		LeaseEpoch: f.mineEpoch, Commit: commit, NativeState: native, Excluded: []string{".env"},
	})
}

func (f *checkpointFixture) projectID() domain.ID {
	f.t.Helper()
	p, err := f.owner.Project(f.t.Context(), f.project)
	must(f.t, err)
	return domain.ID(p.ID)
}

func TestCheckpointRoutes_AClientRequestsAndListsACapture(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()

	got, err := f.owner.RequestCheckpoint(ctx, f.mine.ID, "req-1")
	must(t, err)
	if got.RunState != string(domain.RunCheckpointing) || got.Ref != domain.CheckpointRef(domain.ID(got.CheckpointID)) {
		t.Fatalf("requested = %+v", got)
	}
	again, err := f.owner.RequestCheckpoint(ctx, f.mine.ID, "req-1")
	must(t, err)
	if again != got {
		t.Errorf("a repeated request id answered %+v, then %+v", got, again)
	}
	// A reservation is not listed.
	list, err := f.owner.RunCheckpoints(ctx, f.mine.ID)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("list = %+v", list)
	}

	digest := sha("session")
	must(t, f.upload(f.alpha, f.mine.ID, f.mineEpoch, got.CheckpointID, "session"))
	done, err := f.complete(got.CheckpointID, commitA, &v1.NativeStateRef{Adapter: "claude", AdapterVersion: "2.1", Digest: digest})
	must(t, err)
	if done.RunState != string(domain.RunRunning) {
		t.Errorf("a manual capture left the Run %s", done.RunState)
	}

	byRun, err := f.owner.RunCheckpoints(ctx, f.mine.ID)
	must(t, err)
	task, err := f.owner.Task(ctx, "one", f.project)
	must(t, err)
	byTask, err := f.owner.TaskCheckpoints(ctx, task.Name, f.project)
	must(t, err)
	for _, list := range [][]v1.Checkpoint{byRun, byTask} {
		if len(list) != 1 || list[0].ID != got.CheckpointID || list[0].Trigger != domain.CheckpointManual || list[0].Seq != 1 ||
			list[0].NativeState == nil || list[0].NativeState.Digest != digest || len(list[0].Excluded) != 1 || list[0].Excluded[0] != ".env" {
			t.Fatalf("list = %+v", list)
		}
	}
	other, err := f.owner.TaskCheckpoints(ctx, "two", f.project)
	must(t, err)
	if len(other) != 0 {
		t.Errorf("another Task lists %+v", other)
	}
}

func TestCheckpointRoutes_TheClientRoutesAreForClientsOnly(t *testing.T) {
	f := newCheckpointFixture(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/runs/" + f.mine.ID + "/checkpoints"},
		{http.MethodGet, "/v1/runs/" + f.mine.ID + "/checkpoints"},
		{http.MethodGet, "/v1/tasks/one/checkpoints"},
	} {
		if code, body, _ := f.call(f.httpClientFor(f.alphaD), c.method, c.path, ""); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("%s %s by a Worker: %d %s", c.method, c.path, code, body.Code)
		}
		if code, body, _ := f.call(f.httpClient(), c.method, c.path, ""); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
			t.Errorf("%s %s anonymous: %d %s", c.method, c.path, code, body.Code)
		}
	}
	_, err := f.owner.RunCheckpoints(t.Context(), "run-nope")
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.owner.RequestCheckpoint(t.Context(), "run-nope", "")
	wantCode(t, err, domain.CodeNotFound)
}

func TestCheckpointRoutes_ARunThatIsNotLiveCannotBeCheckpointed(t *testing.T) {
	f := newCheckpointFixture(t)
	f.request()
	// It is checkpointing already: a second capture is not a transition of the machine.
	_, err := f.owner.RequestCheckpoint(t.Context(), f.mine.ID, "")
	wantCode(t, err, domain.CodeInvalidTransition)
}

func TestCheckpointRoutes_TheWorkerRoutesAreForTheWorkerOfTheRun(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	cp := f.request()

	// Clients and anonymous callers have no part in a capture.
	for _, p := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/worker/runs/" + f.mine.ID + "/checkpoint-token", `{}`},
		{http.MethodPost, "/v1/worker/runs/" + f.mine.ID + "/checkpoints/" + cp.CheckpointID + "/complete", `{}`},
		{http.MethodPost, "/v1/worker/runs/" + f.mine.ID + "/checkpoints/" + cp.CheckpointID + "/fail", `{}`},
		{http.MethodPut, "/v1/worker/runs/" + f.mine.ID + "/checkpoints/" + cp.CheckpointID + "/native-state/" + sha("x") + "?lease_epoch=1", "x"},
		{http.MethodGet, "/v1/worker/runs/" + f.mine.ID + "/checkpoints/" + cp.CheckpointID + "/native-state?lease_epoch=1", ""},
	} {
		if code, body, _ := f.call(f.httpClientFor(f.ownerDir), p.method, p.path, p.body); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("%s %s by a client: %d %s", p.method, p.path, code, body.Code)
		}
		if code, body, _ := f.call(f.httpClient(), p.method, p.path, p.body); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
			t.Errorf("%s %s anonymous: %d %s", p.method, p.path, code, body.Code)
		}
	}

	// The Worker of another Run knows nothing of this one.
	_, err := f.beta.CompleteCheckpoint(ctx, f.mine.ID, cp.CheckpointID, v1.CheckpointCompleteRequest{LeaseEpoch: f.theirsEpoch, Commit: commitA})
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.beta.FailCheckpoint(ctx, f.mine.ID, cp.CheckpointID, v1.CheckpointFailRequest{LeaseEpoch: f.theirsEpoch, Reason: "x"})
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.beta.CheckpointToken(ctx, f.mine.ID, v1.CheckpointTokenRequest{LeaseEpoch: f.theirsEpoch, CheckpointID: cp.CheckpointID, Commit: commitA})
	wantCode(t, err, domain.CodeNotFound)
	wantCode(t, f.upload(f.beta, f.mine.ID, f.theirsEpoch, cp.CheckpointID, "x"), domain.CodeNotFound)

	// A Worker with its own Run may not use a checkpoint of another Run.
	wantCode(t, f.upload(f.beta, f.theirs.ID, f.theirsEpoch, cp.CheckpointID, "x"), domain.CodePolicyDenied)
	_, err = f.beta.CheckpointToken(ctx, f.theirs.ID, v1.CheckpointTokenRequest{LeaseEpoch: f.theirsEpoch, CheckpointID: cp.CheckpointID, Commit: commitA})
	wantCode(t, err, domain.CodeNotFound) // the token names a checkpoint of the Run it is for, or none

	// The Worker of the Run gets a token for the ref of its checkpoint alone.
	tok, err := f.alpha.CheckpointToken(ctx, f.mine.ID, v1.CheckpointTokenRequest{LeaseEpoch: f.mineEpoch, CheckpointID: cp.CheckpointID, Commit: commitA})
	must(t, err)
	if tok.Token == "" || tok.Ref != cp.Ref || !tok.Write {
		t.Errorf("token = %+v", tok)
	}
	// Its own epoch only.
	_, err = f.alpha.CheckpointToken(ctx, f.mine.ID, v1.CheckpointTokenRequest{LeaseEpoch: f.mineEpoch + 7, CheckpointID: cp.CheckpointID, Commit: commitA})
	if err == nil {
		t.Error("a token was minted under an epoch the Worker does not hold")
	}
}

func TestCheckpointRoutes_TheNativeStateIsBoundToTheCheckpointItWasUploadedFor(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	cp := f.request()
	native := func(content string) *v1.NativeStateRef {
		return &v1.NativeStateRef{Adapter: "claude", AdapterVersion: "2.1", Digest: sha(content)}
	}

	// A digest that hashes to nothing declared, and one never uploaded.
	_, err := f.alpha.PutNativeState(ctx, f.mine.ID, cp.CheckpointID, f.mineEpoch, sha("promised"), strings.NewReader("other"), 5)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.complete(cp.CheckpointID, commitA, native("never uploaded"))
	wantCode(t, err, domain.CodeInvalidRequest)

	// A blob another Run uploaded is in the store, and is still not this capture's.
	other := f.requestOf(f.beta, f.theirs, f.theirsEpoch)
	must(t, f.upload(f.beta, f.theirs.ID, f.theirsEpoch, other, "their session"))
	_, err = f.complete(cp.CheckpointID, commitA, native("their session"))
	wantCode(t, err, domain.CodeInvalidRequest)

	must(t, f.upload(f.alpha, f.mine.ID, f.mineEpoch, cp.CheckpointID, "mine"))
	_, err = f.complete(cp.CheckpointID, commitA, native("mine"))
	must(t, err)

	// It is only readable by a Worker of the Task, never by the generic blob route.
	rc, _, err := f.alpha.OpenNativeState(ctx, f.mine.ID, cp.CheckpointID, f.mineEpoch)
	must(t, err)
	_ = rc.Close()
	_, _, err = f.beta.OpenNativeState(ctx, f.theirs.ID, cp.CheckpointID, f.theirsEpoch)
	wantCode(t, err, domain.CodePolicyDenied)
	if _, _, err := f.alpha.OpenBlob(ctx, sha("mine")); err == nil {
		t.Error("a Worker read native state through /v1/blobs")
	}
}

// requestOf asks, as the owner, for a capture of a Run held by w and returns its checkpoint.
func (f *checkpointFixture) requestOf(_ *client.Client, run v1.Run, _ uint64) string {
	f.t.Helper()
	out, err := f.owner.RequestCheckpoint(f.t.Context(), run.ID, "")
	must(f.t, err)
	return out.CheckpointID
}

func TestCheckpointRoutes_AFailedCaptureGivesTheRunBack(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	cp := f.request()
	out, err := f.alpha.FailCheckpoint(ctx, f.mine.ID, cp.CheckpointID, v1.CheckpointFailRequest{LeaseEpoch: f.mineEpoch, Reason: "push failed", HarnessAlive: true})
	must(t, err)
	if out.RunState != string(domain.RunRunning) {
		t.Errorf("run = %s", out.RunState)
	}
	list, err := f.owner.RunCheckpoints(ctx, f.mine.ID)
	must(t, err)
	if len(list) != 0 {
		t.Errorf("a failed capture was published: %+v", list)
	}
	// What a Worker reports of an unknown or malformed checkpoint is NotFound.
	_, err = f.alpha.FailCheckpoint(ctx, f.mine.ID, "not-a-checkpoint", v1.CheckpointFailRequest{LeaseEpoch: f.mineEpoch})
	wantCode(t, err, domain.CodeNotFound)
	if code, body, _ := f.call(f.httpClientFor(f.alphaD), http.MethodPut,
		"/v1/worker/runs/"+f.mine.ID+"/checkpoints/"+cp.CheckpointID+"/native-state/"+sha("x"), "x"); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("an upload with no lease epoch: %d %s", code, body.Code)
	}
}
