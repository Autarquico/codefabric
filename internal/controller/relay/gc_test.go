package relay

import (
	"crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

var (
	ids       = domain.NewIDGenerator(time.Now, rand.Reader)
	gcProject = ids.New(domain.KindProject)
	gcRun     = ids.New(domain.KindRun)
)

// gcRelay is a relay holding refs, each at a commit of its own.
func gcRelay(t *testing.T, refs ...string) (*Relay, string) {
	t.Helper()
	gittest.Require(t)
	r, err := Open(t.Context(), Config{Dir: t.TempDir(), HookArgv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := r.Ensure(t.Context(), gcProject)
	if err != nil {
		t.Fatal(err)
	}
	work := gittest.InitRepo(t)
	for i, ref := range refs {
		oid := gittest.Commit(t, work, "f", string(rune('a'+i)), "c")
		gittest.Run(t, dir, "fetch", "--quiet", work, oid)
		gittest.Run(t, dir, "update-ref", ref, oid)
	}
	return r, dir
}

func refsOf(t *testing.T, dir string) []string {
	t.Helper()
	return strings.Fields(gittest.Run(t, dir, "for-each-ref", "--format=%(refname)"))
}

var (
	branchRef     = "refs/heads/cf/task-1/run-1"
	orphanRef     = domain.OrphanRef(gcRun, 3)
	checkpointRef = "refs/cf/checkpoints/run_a-1"
	snapshotRef   = "refs/cf/snapshots/task-1"
	mainRef       = "refs/heads/main"
)

func TestRefsListsOnlyWhatTheCollectorMayDelete(t *testing.T) {
	r, _ := gcRelay(t, branchRef, orphanRef, checkpointRef, snapshotRef, mainRef)
	got, err := r.Refs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got[gcProject])
	if want := []string{orphanRef, branchRef}; !slices.Equal(got[gcProject], want) {
		t.Errorf("Refs = %v, want %v", got[gcProject], want)
	}
}

func TestDeleteRefsRemovesTheBranchAndTheOrphanAndNothingElse(t *testing.T) {
	r, dir := gcRelay(t, branchRef, orphanRef, checkpointRef, snapshotRef, mainRef)
	if err := r.DeleteRefs(t.Context(), gcProject, []string{branchRef, orphanRef}); err != nil {
		t.Fatal(err)
	}
	got := refsOf(t, dir)
	slices.Sort(got)
	if want := []string{checkpointRef, snapshotRef, mainRef}; !slices.Equal(got, want) {
		t.Errorf("refs left = %v, want %v", got, want)
	}
}

func TestDeleteRefsRefusesTheNamespacesItDoesNotOwnBeforeDeletingAny(t *testing.T) {
	for _, foreign := range []string{checkpointRef, snapshotRef, mainRef, "refs/heads/cf/x"} {
		r, dir := gcRelay(t, branchRef, foreign)
		if err := r.DeleteRefs(t.Context(), gcProject, []string{branchRef, foreign}); err == nil {
			t.Errorf("DeleteRefs(%s) = nil", foreign)
		}
		if got := refsOf(t, dir); len(got) != 2 {
			t.Errorf("%s: refs left = %v, want both", foreign, got)
		}
	}
}

func TestDeleteRefsRemovesTheRefOfACheckpointByName(t *testing.T) {
	cp := domain.CheckpointRef(ids.New(domain.KindCheckpoint))
	r, dir := gcRelay(t, cp, snapshotRef)
	if err := r.DeleteRefs(t.Context(), gcProject, []string{cp}); err != nil {
		t.Fatal(err)
	}
	if got := refsOf(t, dir); !slices.Equal(got, []string{snapshotRef}) {
		t.Errorf("refs left = %v, want only the snapshot", got)
	}
}

func TestCollectWaitsWhileAPushIsInFlight(t *testing.T) {
	r, _ := gcRelay(t, branchRef)
	r.pushing["worker_1"] = struct{}{}
	if ok, err := r.Collect(t.Context(), gcProject); ok || err != nil {
		t.Errorf("Collect during a push = %v, %v", ok, err)
	}
	delete(r.pushing, "worker_1")
	if ok, err := r.Collect(t.Context(), gcProject); !ok || err != nil {
		t.Errorf("Collect = %v, %v", ok, err)
	}
}
