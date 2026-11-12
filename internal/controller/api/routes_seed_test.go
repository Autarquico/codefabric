package api_test

import (
	"crypto/rand"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// seedFixture is a Controller with two Workers and a Project that keeps native state.
type seedFixture struct {
	*testController
	work          *app.Services
	alpha, beta   *client.Client
	alphaD, betaD string
}

const seedProject = "backend"

func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()
	f := &seedFixture{}
	f.testController = newController(t, time.Now(), func(c *api.Config) {
		bs, err := blob.New(t.TempDir(), c.Clock)
		must(t, err)
		c.Work = app.New(c.Store.(*store.DB), c.Clock, domain.NewIDGenerator(c.Clock.Now, rand.Reader),
			app.Config{Workers: workers.Config{ControllerVersion: controllerVersion}, Blobs: bs})
		f.work = c.Work
	})
	ctx := t.Context()
	f.alpha, f.alphaD = f.enrollRealWorker("alpha")
	f.beta, f.betaD = f.enrollRealWorker("beta")
	for _, w := range []*client.Client{f.alpha, f.beta} {
		_, err := w.Heartbeat(ctx, heartbeat(newEpoch()))
		must(t, err)
	}
	_, err := f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: seedProject, Repo: "/srv/r"})
	must(t, err)
	keep := true
	_, err = f.owner.UpdateProject(ctx, seedProject, v1.UpdateProjectRequest{NativeState: &keep})
	must(t, err)
	return f
}

func (f *seedFixture) upload(content string) *v1.NativeStateRef {
	f.t.Helper()
	ref := &v1.NativeStateRef{Adapter: "claude", AdapterVersion: "2.1", Digest: sha(content)}
	_, err := f.owner.PutNativeSeed(f.t.Context(), seedProject, ref.Digest, strings.NewReader(content), int64(len(content)))
	must(f.t, err)
	return ref
}

// run creates a Task, seeded if seed is not nil, and returns its Run, the Worker the
// scheduler gave it to and the lease.
func (f *seedFixture) run(name string, seed *v1.NativeStateRef) (v1.Run, *client.Client, v1.AssignCommand) {
	f.t.Helper()
	ctx := f.t.Context()
	created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{
		Project: seedProject, Name: name, Prompt: "x", ResolvedRef: "main", SourceCommit: commitA, NativeSeed: seed,
	})
	must(f.t, err)
	_, err = f.work.Scheduler.Pass(ctx)
	must(f.t, err)
	for _, w := range []*client.Client{f.alpha, f.beta} {
		page, err := w.Assignments(ctx, 0, 0, 0)
		must(f.t, err)
		for _, cmd := range page.Commands {
			if a := cmd.Assign; a != nil && a.RunID == created.Run.ID {
				_, err := w.AcceptLease(ctx, a.LeaseID, a.Epoch)
				must(f.t, err)
				return *created.Run, w, *a
			}
		}
	}
	f.t.Fatal("no Worker was given the Run")
	return v1.Run{}, nil, v1.AssignCommand{}
}

func (f *seedFixture) otherThan(w *client.Client) *client.Client {
	if w == f.alpha {
		return f.beta
	}
	return f.alpha
}

func TestSeedRoutes_AClientUploadsAndTheWorkerOfTheRunDownloads(t *testing.T) {
	f := newSeedFixture(t)
	ctx := t.Context()
	seed := f.upload("the session")
	run, holder, a := f.run("seeded", seed)

	if c := a.Continues; c == nil || c.CheckpointID != "" || c.Ref != "" {
		t.Fatalf("continues = %+v: a seed needs no checkpoint", c)
	}
	rc, size, err := holder.OpenNativeSeed(ctx, run.ID, a.Epoch)
	must(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "the session" || size != int64(len(got)) {
		t.Fatalf("served %q (%d)", got, size)
	}

	_, _, err = f.otherThan(holder).OpenNativeSeed(ctx, run.ID, a.Epoch)
	wantCode(t, err, domain.CodeNotFound)
	if _, _, err := holder.OpenNativeSeed(ctx, run.ID, a.Epoch+7); err == nil {
		t.Error("a seed was served under an epoch the Worker does not hold")
	}
	if _, _, err := holder.OpenBlob(ctx, seed.Digest); err == nil {
		t.Error("a Worker read a session seed through /v1/blobs")
	}
}

func TestSeedRoutes_ARunThatWasNotSeededHasNone(t *testing.T) {
	f := newSeedFixture(t)
	run, holder, a := f.run("plain", nil)
	_, _, err := holder.OpenNativeSeed(t.Context(), run.ID, a.Epoch)
	wantCode(t, err, domain.CodeNotFound)
}

func TestSeedRoutes_AreForTheRightIdentity(t *testing.T) {
	f := newSeedFixture(t)
	f.upload("x")
	run, _, _ := f.run("t", nil)
	for _, c := range []struct {
		method, path, body string
		client             bool // the identity that may call it
	}{
		{http.MethodPut, "/v1/projects/" + seedProject + "/native-seed/" + sha("x"), "x", true},
		{http.MethodPost, "/v1/tasks/t/pulled", `{}`, true},
		{http.MethodGet, "/v1/worker/runs/" + run.ID + "/native-seed?lease_epoch=1", "", false},
	} {
		wrong, right := f.httpClientFor(f.alphaD), f.httpClientFor(f.ownerDir)
		if !c.client {
			wrong, right = right, wrong
		}
		if code, body, _ := f.call(wrong, c.method, c.path, c.body); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("%s %s by the wrong identity: %d %s", c.method, c.path, code, body.Code)
		}
		if code, body, _ := f.call(f.httpClient(), c.method, c.path, c.body); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
			t.Errorf("%s %s anonymous: %d %s", c.method, c.path, code, body.Code)
		}
		if code, _, _ := f.call(right, c.method, c.path, c.body); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Errorf("%s %s by the right identity: %d", c.method, c.path, code)
		}
	}
}

func TestSeedRoutes_TheUploadIsCheckedAgainstItsDigestAndTheProject(t *testing.T) {
	f := newSeedFixture(t)
	ctx := t.Context()
	_, err := f.owner.PutNativeSeed(ctx, seedProject, sha("promised"), strings.NewReader("other"), 5)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.owner.PutNativeSeed(ctx, seedProject, "not-a-digest", strings.NewReader("x"), 1)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.owner.PutNativeSeed(ctx, "nope", sha("x"), strings.NewReader("x"), 1)
	wantCode(t, err, domain.CodeNotFound)

	off := false
	_, err = f.owner.UpdateProject(ctx, seedProject, v1.UpdateProjectRequest{NativeState: &off})
	must(t, err)
	_, err = f.owner.PutNativeSeed(ctx, seedProject, sha("x"), strings.NewReader("x"), 1)
	wantCode(t, err, domain.CodePolicyDenied)
}
