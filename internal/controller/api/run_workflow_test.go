package api_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// workflowRunFixture is a Run that froze version 1 of the Workflow "team",
// leased (pending) to one of two Workers: holder; other is the second.
type workflowRunFixture struct {
	*testController
	work          *app.Services
	holder, other *client.Client
	holderD       string
	run           v1.Run
	assign        v1.AssignCommand
	content       string
}

func newWorkflowRunFixture(t *testing.T) *workflowRunFixture {
	t.Helper()
	f := &workflowRunFixture{content: "Always answer in Spanish.\n"}
	f.testController = newController(t, time.Now(), withBlobs(t), func(c *api.Config) { f.work = c.Work })
	ctx := t.Context()
	alpha, alphaD := f.enrollRealWorker("alpha")
	beta, betaD := f.enrollRealWorker("beta")
	for _, w := range []*client.Client{alpha, beta} {
		beat := heartbeat(newEpoch())
		beat.Capabilities = append(beat.Capabilities, domain.WorkflowConfigCapability)
		_, err := w.Heartbeat(ctx, beat)
		must(t, err)
	}
	_, err := f.owner.PutBlob(ctx, digestOf(f.content), strings.NewReader(f.content), int64(len(f.content)))
	must(t, err)
	_, err = f.owner.CommitWorkflow(ctx, "team", v1.CommitWorkflowRequest{Manifest: wfManifest(t, f.content), Changelog: "c"})
	must(t, err)
	_, err = f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)
	created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA, Workflow: "team"})
	must(t, err)
	f.run = *created.Run
	_, err = f.work.Scheduler.Pass(ctx)
	must(t, err)
	for _, w := range []struct {
		c   *client.Client
		dir string
	}{{alpha, alphaD}, {beta, betaD}} {
		page, err := w.c.Assignments(ctx, 0, 0, 0)
		must(t, err)
		if len(page.Commands) == 1 {
			f.assign, f.holder, f.holderD = *page.Commands[0].Assign, w.c, w.dir
		} else {
			f.other = w.c
		}
	}
	if f.holder == nil || f.other == nil {
		t.Fatal("the Run was not leased to exactly one Worker")
	}
	return f
}

func codeOf(err error) domain.Code {
	var coded *domain.Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	must(t, err)
	return string(b)
}

func TestRunWorkflowApi_TheHolderOfALeaseReadsTheFrozenWorkflow(t *testing.T) {
	f := newWorkflowRunFixture(t)
	ctx := t.Context()
	// Pending lease: the Worker checks the secrets of the configuration before it accepts.
	for _, accepted := range []bool{false, true} {
		if accepted {
			_, err := f.holder.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
			must(t, err)
		}
		v, err := f.holder.RunWorkflow(ctx, f.run.ID, f.assign.Epoch)
		must(t, err)
		if v.Workflow != "team" || v.Version != 1 || v.Manifest == nil || v.Digest != f.run.Workflow.Digest {
			t.Fatalf("accepted=%v: manifest = %+v", accepted, v)
		}
		body, size, err := f.holder.OpenRunBlob(ctx, f.run.ID, f.assign.Epoch, digestOf(f.content))
		must(t, err)
		if got := readAll(t, body); got != f.content || size != int64(len(f.content)) {
			t.Fatalf("accepted=%v: blob = %q (%d bytes)", accepted, got, size)
		}
	}
}

func TestRunWorkflowApi_EveryoneElseIsRefused(t *testing.T) {
	f := newWorkflowRunFixture(t)
	ctx := t.Context()
	d := digestOf(f.content)

	// Another Worker holds no lease on the Run.
	if _, err := f.other.RunWorkflow(ctx, f.run.ID, f.assign.Epoch); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("beta manifest: %v", err)
	}
	if _, _, err := f.other.OpenRunBlob(ctx, f.run.ID, f.assign.Epoch, d); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("beta blob: %v", err)
	}
	// An epoch the Worker never held.
	if _, err := f.holder.RunWorkflow(ctx, f.run.ID, f.assign.Epoch+7); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("an epoch never issued: %v", err)
	}
	// A blob outside the manifest, though the store has it: the owner uploads it.
	const other = "someone else's file"
	_, err := f.owner.PutBlob(ctx, digestOf(other), strings.NewReader(other), int64(len(other)))
	must(t, err)
	if _, _, err := f.holder.OpenRunBlob(ctx, f.run.ID, f.assign.Epoch, digestOf(other)); codeOf(err) != domain.CodeNotFound {
		t.Errorf("a blob of no manifest of the Run: %v", err)
	}
	// Clients keep their routes, and are not on the Worker's.
	if _, err := f.owner.ShowWorkflow(ctx, "team"); err != nil {
		t.Errorf("client show: %v", err)
	}
	if body, _, err := f.owner.OpenBlob(ctx, d); err != nil {
		t.Errorf("client blob: %v", err)
	} else {
		_ = body.Close()
	}
	if _, err := f.owner.RunWorkflow(ctx, f.run.ID, f.assign.Epoch); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("client on the Worker route: %v", err)
	}
	// The Worker still has no way into the store at large.
	if _, _, err := f.holder.OpenBlob(ctx, d); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("Worker on the client blob route: %v", err)
	}
	anon := f.httpClient()
	if s, _, _ := f.call(anon, http.MethodGet, "/v1/runs/"+f.run.ID+"/workflow?lease_epoch=1", ""); s != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", s)
	}
	// The fence is not optional.
	alpha := f.httpClientFor(f.holderD)
	for _, q := range []string{"", "?lease_epoch=0", "?lease_epoch=x"} {
		if s, e, _ := f.call(alpha, http.MethodGet, "/v1/runs/"+f.run.ID+"/workflow"+q, ""); s != http.StatusBadRequest || e.Code != domain.CodeInvalidRequest {
			t.Errorf("query %q = %d %s, want 400", q, s, e.Code)
		}
	}
}

func TestRunWorkflowApi_ALeaseThatEndedIsStale(t *testing.T) {
	f := newWorkflowRunFixture(t)
	ctx := t.Context()
	_, err := f.holder.RejectLease(ctx, f.assign.LeaseID, v1.RejectLeaseRequest{Epoch: f.assign.Epoch, Code: domain.CodeWorkerCapacity, Message: "no room"})
	must(t, err)
	if _, err := f.holder.RunWorkflow(ctx, f.run.ID, f.assign.Epoch); codeOf(err) != domain.CodeStaleLease {
		t.Errorf("manifest under a rejected lease: %v", err)
	}
	if _, _, err := f.holder.OpenRunBlob(ctx, f.run.ID, f.assign.Epoch, digestOf(f.content)); codeOf(err) != domain.CodeStaleLease {
		t.Errorf("blob under a rejected lease: %v", err)
	}
}

func TestRunWorkflowApi_ARunWithoutWorkflowHasNothingToRead(t *testing.T) {
	f := newWorkflowRunFixture(t)
	ctx := t.Context()
	// The holder is at 2 of 8 CPUs: the plain Run is leased too, to whoever the scheduler picks.
	created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Prompt: "plain", ResolvedRef: "main", SourceCommit: commitA})
	must(t, err)
	_, err = f.work.Scheduler.Pass(ctx)
	must(t, err)
	for _, w := range []*client.Client{f.holder, f.other} {
		page, err := w.Assignments(ctx, 0, 0, 0)
		must(t, err)
		for _, c := range page.Commands {
			if c.Assign != nil && c.Assign.RunID == created.Run.ID {
				if _, err := w.RunWorkflow(ctx, created.Run.ID, c.Assign.Epoch); codeOf(err) != domain.CodeNotFound {
					t.Errorf("a Run that froze no Workflow: %v", err)
				}
				return
			}
		}
	}
	t.Fatal("the plain Run was not leased")
}

// The digest the Worker reports with `started` is kept with the Run and read
// back from it.
func TestRunWorkflowApi_TheDigestOfTheMaterialisationIsRecordedWithStarted(t *testing.T) {
	f := newWorkflowRunFixture(t)
	ctx := t.Context()
	_, err := f.holder.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
	must(t, err)
	digest := digestOf("the materialised tree")
	out, err := f.holder.ReportRun(ctx, f.run.ID, v1.RunReportRequest{Entries: []v1.RunReportEntry{{
		WorkerEpoch: newEpoch(), WorkerSeq: 1, LeaseEpoch: f.assign.Epoch, Kind: v1.ReportStarted, At: time.Now(),
		Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-1"}, ConfigDigest: digest,
	}}})
	must(t, err)
	if out.Outcomes[0].Result != v1.ReportApplied {
		t.Fatalf("outcome = %+v", out.Outcomes[0])
	}
	got, err := f.owner.Run(ctx, f.run.ID)
	must(t, err)
	if got.EffectiveConfigDigest != digest {
		t.Errorf("effective_config_digest = %q, want %q", got.EffectiveConfigDigest, digest)
	}
}

// bothRoutes asserts the code both Worker routes answer for the Run.
func (f *workflowRunFixture) bothRoutes(t *testing.T, label string, epoch uint64, want domain.Code) {
	t.Helper()
	ctx := t.Context()
	if _, err := f.holder.RunWorkflow(ctx, f.run.ID, epoch); codeOf(err) != want {
		t.Errorf("%s, manifest: %v, want %s", label, err, want)
	}
	if _, _, err := f.holder.OpenRunBlob(ctx, f.run.ID, epoch, digestOf(f.content)); codeOf(err) != want {
		t.Errorf("%s, blob: %v, want %s", label, err, want)
	}
}

// exec changes the Controller's rows behind its back, to reach a deny
// condition of the fence on its own.
func (f *workflowRunFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	must(t, f.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), query, args...)
		return err
	}))
}

func TestRunWorkflowApi_TheFenceDeniesWhatIsNoLongerTheLiveLease(t *testing.T) {
	t.Run("a pending lease past accept_by", func(t *testing.T) {
		f := newWorkflowRunFixture(t)
		f.bothRoutes(t, "before", f.assign.Epoch, "")
		f.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
		f.bothRoutes(t, "after", f.assign.Epoch, domain.CodeStaleLease)
	})
	t.Run("an accepted lease does not expire", func(t *testing.T) {
		f := newWorkflowRunFixture(t)
		_, err := f.holder.AcceptLease(t.Context(), f.assign.LeaseID, f.assign.Epoch)
		must(t, err)
		f.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
		f.bothRoutes(t, "accepted", f.assign.Epoch, "")
	})
	t.Run("a Run that ended", func(t *testing.T) {
		f := newWorkflowRunFixture(t)
		_, err := f.holder.AcceptLease(t.Context(), f.assign.LeaseID, f.assign.Epoch)
		must(t, err)
		f.exec(t, `UPDATE runs SET state = ? WHERE id = ?`, string(domain.RunCancelled), f.run.ID)
		f.bothRoutes(t, "terminal", f.assign.Epoch, domain.CodeStaleLease)
	})
	t.Run("a lease the Run moved past", func(t *testing.T) {
		f := newWorkflowRunFixture(t)
		_, err := f.holder.AcceptLease(t.Context(), f.assign.LeaseID, f.assign.Epoch)
		must(t, err)
		f.exec(t, `UPDATE runs SET lease_epoch = ? WHERE id = ?`, f.assign.Epoch+1, f.run.ID)
		f.bothRoutes(t, "superseded", f.assign.Epoch, domain.CodeStaleLease)
	})
	t.Run("a Task archived under a live lease", func(t *testing.T) {
		f := newWorkflowRunFixture(t)
		_, err := f.holder.AcceptLease(t.Context(), f.assign.LeaseID, f.assign.Epoch)
		must(t, err)
		f.exec(t, `UPDATE tasks SET archived_at = ? WHERE id = (SELECT task_id FROM runs WHERE id = ?)`, f.clock.Now().UnixNano(), f.run.ID)
		f.bothRoutes(t, "archived", f.assign.Epoch, domain.CodeNotFound)
	})
}
