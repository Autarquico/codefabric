package gc_test

import (
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// realRelay is the Controller's relay on a temp dir, with a repository for a
// Project the database knows, and a way to put refs in it.
type realRelay struct {
	*relay.Relay
	t       *testing.T
	project domain.ID
	dir     string
	work    string
	commits int
}

func (f *fixture) realRelay() *realRelay {
	f.t.Helper()
	gittest.Require(f.t)
	r, err := relay.Open(f.t.Context(), relay.Config{Dir: f.t.TempDir(), HookArgv: []string{"/bin/true"}})
	if err != nil {
		f.t.Fatal(err)
	}
	project := domain.NewIDGenerator(time.Now, rand.Reader).New(domain.KindProject)
	dir, err := r.Ensure(f.t.Context(), project)
	if err != nil {
		f.t.Fatal(err)
	}
	f.tx(func(tx *store.Tx) error {
		return tx.InsertProject(f.t.Context(), store.Project{
			ID: project, Short: 2, Name: "relayed", Repo: "r2", DefaultBranch: "main", CreatedAt: epoch,
		})
	})
	return &realRelay{Relay: r, t: f.t, project: project, dir: dir, work: gittest.InitRepo(f.t)}
}

func (r *realRelay) put(ref string) {
	r.t.Helper()
	r.commits++
	oid := gittest.Commit(r.t, r.work, "f", fmt.Sprint(r.commits), "c")
	gittest.Run(r.t, r.dir, "fetch", "--quiet", r.work, oid)
	gittest.Run(r.t, r.dir, "update-ref", ref, oid)
}

func (r *realRelay) refs() []string {
	r.t.Helper()
	out := strings.Fields(gittest.Run(r.t, r.dir, "for-each-ref", "--format=%(refname)=%(objectname)"))
	slices.Sort(out)
	return out
}

// task inserts a Task of the relay's Project with one Run, archived archivedAgo
// days before the clock if that is positive. It puts the branch and the orphan
// ref of the Run in the relay and returns them.
func (r *realRelay) task(f *fixture, n uint64, name string, archivedDays int) (branch, orphan string) {
	f.t.Helper()
	task := domain.ID("task_" + name)
	f.tx(func(tx *store.Tx) error {
		return tx.InsertTask(f.t.Context(), store.Task{
			ID: task, Short: n, ProjectID: r.project, Name: name, Prompt: "p",
			State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: epoch,
		})
	})
	f.runs++
	run := domain.NewIDGenerator(time.Now, rand.Reader).New(domain.KindRun)
	f.tx(func(tx *store.Tx) error {
		return tx.InsertRun(f.t.Context(), store.Run{
			ID: run, Short: uint64(f.runs), TaskID: task, Attempt: 1, Lineage: domain.LineageInitial, //nolint:gosec // small counter
			Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch,
		})
	})
	if archivedDays > 0 {
		f.exec(`UPDATE tasks SET archived_at = ? WHERE id = ?`, f.clk.Now().Add(-time.Duration(archivedDays)*day).UnixNano(), string(task))
	}
	branch = "refs/heads/cf/" + name + "/run-" + fmt.Sprint(f.runs)
	orphan = domain.OrphanRef(run, 2)
	r.put(branch)
	r.put(orphan)
	return branch, orphan
}

const (
	checkpointRef = "refs/cf/checkpoints/run_a-1"
	snapshotRef   = "refs/cf/snapshots/task_old"
)

// Dry run against a real relay: it lists what the pass then deletes, from blobs,
// history and relay alike, and changes none of them.
func TestScenario_RetentionGc_DryRunLeavesBlobsEventsAndRelayAlone(t *testing.T) {
	f := newFixture(t)
	rel := f.realRelay()
	rel.task(f, 10, "old", 40)
	rel.task(f, 11, "fresh", 0)
	rel.put(checkpointRef)
	rel.put(snapshotRef)
	for range 4 {
		f.event("")
	}
	orphanBlob := f.blob("nobody wants this", 5*day)
	f.clk.Advance(10 * day)
	f.event("")
	svc := f.service(shortPolicy, gc.WithRelay(rel.Relay))

	refsBefore, eventsBefore := rel.refs(), f.count("events")
	dry, err := svc.Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := items(dry); got["events"] != 4 || got["blobs"] != 1 || got["relay_refs"] != 2 {
		t.Fatalf("dry run = %+v", dry.Items)
	}
	if !slices.Equal(rel.refs(), refsBefore) || f.count("events") != eventsBefore || !f.has(orphanBlob) {
		t.Fatal("a dry run changed the relay, the events or the blobs")
	}

	applied, err := svc.Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied.Items, dry.Items) {
		t.Fatalf("the pass deleted %+v, the dry run listed %+v", applied.Items, dry.Items)
	}
	if f.count("events") != 1 || f.has(orphanBlob) || len(rel.refs()) != len(refsBefore)-2 {
		t.Fatalf("after the pass: %d events, blob present %v, refs %v", f.count("events"), f.has(orphanBlob), rel.refs())
	}
}

// What the relay holds for a live Task, and the namespaces the GC does not own,
// survive a pass that does delete the refs of an archived Task.
func TestScenario_RetentionGc_ReferenciasVivasInTheRelay(t *testing.T) {
	f := newFixture(t)
	rel := f.realRelay()
	oldBranch, oldOrphan := rel.task(f, 10, "old", 40)
	liveBranch, liveOrphan := rel.task(f, 11, "live", 0)
	rel.put(checkpointRef)
	rel.put(snapshotRef)
	rel.put("refs/heads/main")
	before := rel.refs()

	rep, err := f.service(shortPolicy, gc.WithRelay(rel.Relay)).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if items(rep)["relay_refs"] != 2 {
		t.Fatalf("relay_refs = %d, want the branch and the orphan of the archived Task", items(rep)["relay_refs"])
	}
	left := map[string]bool{}
	for _, r := range rel.refs() {
		left[strings.SplitN(r, "=", 2)[0]] = true
	}
	for _, kept := range []string{liveBranch, liveOrphan, checkpointRef, snapshotRef, "refs/heads/main"} {
		if !left[kept] {
			t.Errorf("%s was deleted", kept)
		}
	}
	for _, gone := range []string{oldBranch, oldOrphan} {
		if left[gone] {
			t.Errorf("%s is still there", gone)
		}
	}
	// The refs that stay point at the commits they pointed at.
	for _, b := range before {
		name := strings.SplitN(b, "=", 2)[0]
		if left[name] && !slices.Contains(rel.refs(), b) {
			t.Errorf("%s moved", name)
		}
	}
}

// Every batch of refs deleted from the relay is at most gc.batch_size, and
// the refs are gone batch by batch (each deletion is visible before the next).
func TestScenario_RetentionGc_LatenciaRelayRefsInBatches(t *testing.T) {
	const batch = 4
	f := newFixture(t)
	rel := f.realRelay()
	for i := range 5 {
		rel.task(f, uint64(10+i), fmt.Sprintf("old%d", i), 40) // 10 refs
	}
	left := len(rel.refs())
	var batches int
	hooks := gc.Hooks{AfterBatch: func(kind string, rows int64) {
		if kind != "relay_refs" {
			return
		}
		batches++
		now := len(rel.refs())
		if rows > batch || left-now != int(rows) {
			t.Errorf("batch %d: %d rows (limit %d), but %d refs left the relay", batches, rows, batch, left-now)
		}
		left = now
	}}
	policy := shortPolicy
	policy.BatchSize = batch
	if _, err := f.service(policy, gc.WithRelay(rel.Relay), gc.WithHooks(hooks)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if batches != 3 || left != 0 {
		t.Fatalf("%d batches, %d refs left; want 3 batches (4+4+2) and none", batches, left)
	}
}
