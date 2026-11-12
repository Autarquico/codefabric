package gc_test

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// fakeRelay holds refs by Project and records what is asked of it.
type fakeRelay struct {
	refs      map[domain.ID][]string
	deleted   []string
	collected []domain.ID
}

func (r *fakeRelay) Refs(context.Context) (map[domain.ID][]string, error) { return r.refs, nil }

func (r *fakeRelay) DeleteRefs(_ context.Context, p domain.ID, refs []string) error {
	r.deleted = append(r.deleted, refs...)
	r.refs[p] = slices.DeleteFunc(r.refs[p], func(n string) bool { return slices.Contains(refs, n) })
	return nil
}

func (r *fakeRelay) Collect(_ context.Context, p domain.ID) (bool, error) {
	r.collected = append(r.collected, p)
	return true, nil
}

// taskRuns inserts a Task named name with one Run, archived archivedAgo ago if
// that is positive, and returns the branch and the orphan ref of the Run.
func (f *fixture) taskRuns(n uint64, name string, archivedAgo int) (branch, orphan string) {
	f.t.Helper()
	task := domain.ID("task_" + name)
	f.tx(func(tx *store.Tx) error {
		return tx.InsertTask(f.t.Context(), store.Task{
			ID: task, Short: n, ProjectID: "project_1", Name: name, Prompt: "p",
			State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: epoch,
		})
	})
	f.runs++
	run := domain.ID("run_" + name)
	f.tx(func(tx *store.Tx) error {
		return tx.InsertRun(f.t.Context(), store.Run{
			ID: run, Short: uint64(f.runs), TaskID: task, Attempt: 1, Lineage: domain.LineageInitial, //nolint:gosec // small counter
			Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch,
		})
	})
	if archivedAgo > 0 {
		f.exec(`UPDATE tasks SET archived_at = ? WHERE id = ?`, epoch.Add(-time.Duration(archivedAgo)*day).UnixNano(), string(task))
	}
	return "refs/heads/cf/" + name + "/run-" + itoa(f.runs), "refs/cf/orphans/" + string(run) + "-e2"
}

func itoa(n int) string { return strconv.Itoa(n) }

const relayProject = domain.ID("project_1")

func TestRelayGC_DeletesTheRefsOfArchivedTasksAndNothingElse(t *testing.T) {
	f := newFixture(t)
	oldBranch, oldOrphan := f.taskRuns(10, "old", 31)
	freshBranch, freshOrphan := f.taskRuns(11, "fresh", 29)
	liveBranch, liveOrphan := f.taskRuns(12, "live", 0)
	foreign := []string{"refs/cf/checkpoints/run_old-1", "refs/cf/snapshots/task_old", "refs/heads/main", "refs/heads/cf/old/run-999"}
	rel := &fakeRelay{refs: map[domain.ID][]string{
		relayProject: append([]string{oldBranch, oldOrphan, freshBranch, freshOrphan, liveBranch, liveOrphan}, foreign...),
	}}

	rep, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{oldOrphan, oldBranch}; !slices.Equal(rel.deleted, want) {
		t.Errorf("deleted %v, want %v", rel.deleted, want)
	}
	if got := items(rep)["relay_refs"]; got != 2 {
		t.Errorf("relay_refs = %d, want 2", got)
	}
	if !slices.Equal(rel.collected, []domain.ID{relayProject}) {
		t.Errorf("collected %v", rel.collected)
	}
}

// The plan is some time old when it is carried out: a Task unarchived in between keeps
// its refs.
func TestRelayGC_ATaskUnarchivedAfterThePlanKeepsItsRefs(t *testing.T) {
	f := newFixture(t)
	branch, orphan := f.taskRuns(10, "old", 31)
	rel := &fakeRelay{refs: map[domain.ID][]string{relayProject: {branch, orphan}}}
	hooks := gc.Hooks{BeforeDeleteRefs: func(domain.ID, []string) {
		f.exec(`UPDATE tasks SET archived_at = NULL WHERE name = 'old'`)
	}}

	rep, err := f.service(gc.Policy{}, gc.WithRelay(rel), gc.WithHooks(hooks)).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.deleted) != 0 || items(rep)["relay_refs"] != 0 {
		t.Errorf("deleted %v, relay_refs = %d", rel.deleted, items(rep)["relay_refs"])
	}
}

func TestRelayGC_KeepsTheRefsOfARunWhosePublicationIsPending(t *testing.T) {
	f := newFixture(t)
	branch, _ := f.taskRuns(10, "old", 31)
	f.exec(`UPDATE runs SET publication = 'pending' WHERE id = 'run_old'`)
	rel := &fakeRelay{refs: map[domain.ID][]string{relayProject: {branch}}}
	if _, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if len(rel.deleted) != 0 || len(rel.collected) != 0 {
		t.Errorf("deleted %v, collected %v", rel.deleted, rel.collected)
	}
}

func TestRelayGC_DryRunReportsWithoutDeletingOrCollecting(t *testing.T) {
	f := newFixture(t)
	branch, orphan := f.taskRuns(10, "old", 31)
	rel := &fakeRelay{refs: map[domain.ID][]string{relayProject: {branch, orphan}}}
	svc := f.service(gc.Policy{}, gc.WithRelay(rel))

	rep, err := svc.Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := items(rep)["relay_refs"]; got != 2 {
		t.Errorf("relay_refs = %d, want 2", got)
	}
	if len(rel.deleted) != 0 || len(rel.collected) != 0 || len(rel.refs[relayProject]) != 2 {
		t.Errorf("a dry run changed the relay: deleted %v, collected %v", rel.deleted, rel.collected)
	}
	stats, err := svc.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := items(stats.Reclaimable)["relay_refs"]; got != 2 {
		t.Errorf("stats reclaimable relay_refs = %d, want 2", got)
	}
}

func TestRelayGC_RelayBranchesRetentionIsConfigurable(t *testing.T) {
	f := newFixture(t)
	branch, _ := f.taskRuns(10, "mid", 10)
	rel := &fakeRelay{refs: map[domain.ID][]string{relayProject: {branch}}}
	if _, err := f.service(gc.Policy{RelayBranches: 7 * day}, gc.WithRelay(rel)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rel.deleted, []string{branch}) {
		t.Errorf("deleted %v", rel.deleted)
	}
}
