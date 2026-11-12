package gc_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/domain"
)

// seed records a session seed uploaded age ago and returns its blob, uploaded then too.
func (f *fixture) seed(content string, age time.Duration) string {
	f.t.Helper()
	d := f.blob(content, age)
	f.exec(`INSERT INTO native_seeds (digest, project_id, uploaded_by, created_at) VALUES (?, 'project_1', 'c', ?)`,
		string(d), f.clk.Now().Add(-age).UnixNano())
	return string(d)
}

func (f *fixture) seeded(run domain.ID, digest string) {
	f.t.Helper()
	f.exec(`UPDATE runs SET seed_adapter = 'claude', seed_adapter_version = '1', seed_digest = ? WHERE id = ?`, digest, string(run))
}

func (f *fixture) seedRows() int { return f.count("native_seeds") }

func TestSeedGC_KeepsTheSeedOfARunThatRunsAndOfATaskNotArchived(t *testing.T) {
	f := newFixture(t)
	f.seeded(f.run(false), f.seed("running", 30*day))
	f.seeded(f.run(true), f.seed("ended-of-a-live-task", 30*day))

	rep, err := f.service(shortPolicy).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := items(rep)["native_seeds"]; got != 0 {
		t.Errorf("deleted %d seeds, want 0", got)
	}
	if !f.has(digestOf("running")) || !f.has(digestOf("ended-of-a-live-task")) || f.seedRows() != 2 {
		t.Error("a seed a live Run or Task keeps was deleted")
	}
}

func TestSeedGC_DeletesTheSeedOfAnArchivedTaskAndItsBlob(t *testing.T) {
	f := newFixture(t)
	f.seeded(f.run(true), f.seed("done", 30*day))
	f.exec(`UPDATE tasks SET archived_at = ? WHERE id = 'task_1'`, epoch.Add(-day).UnixNano())

	rep, err := f.service(shortPolicy).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if items(rep)["native_seeds"] != 1 || f.seedRows() != 0 {
		t.Errorf("report %v, rows %d: the seed stayed", items(rep), f.seedRows())
	}
	if f.has(digestOf("done")) {
		t.Error("the blob of the seed stayed")
	}
}

func TestSeedGC_AnUnnamedSeedHasTheGraceOfAReservation(t *testing.T) {
	f := newFixture(t)
	f.seed("fresh", 6*day)
	f.seed("stale", 8*day)

	if _, err := f.service(shortPolicy).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if f.seedRows() != 1 || !f.has(digestOf("fresh")) {
		t.Errorf("rows %d: want only the fresh seed (7 days of grace)", f.seedRows())
	}
	f.clk.Advance(2 * day)
	if _, err := f.service(shortPolicy).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if f.seedRows() != 0 {
		t.Errorf("rows %d after the grace, want 0", f.seedRows())
	}
}

func TestSeedGC_DryRunCountsAndDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.seed("stale", 8*day)
	rep, err := f.service(shortPolicy).Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if items(rep)["native_seeds"] != 1 || f.seedRows() != 1 {
		t.Errorf("report %v, rows %d", items(rep), f.seedRows())
	}
}

func TestSnapshotGC_DeletesTheSnapshotsOnlyArchivedTasksUse(t *testing.T) {
	f := newFixture(t)
	sha := func(c string) string { return strings.Repeat(c, 40) }
	f.taskRuns(10, "old", 31)
	f.taskRuns(11, "fresh", 29)
	f.taskRuns(12, "live", 0)
	f.exec(`UPDATE runs SET source_commit = ? WHERE id = 'run_old'`, sha("a"))
	f.exec(`UPDATE runs SET source_commit = ? WHERE id = 'run_fresh'`, sha("b"))
	f.exec(`UPDATE tasks SET source_snapshot = ? WHERE id = 'task_live'`, sha("c"))
	f.exec(`UPDATE runs SET source_commit = ? WHERE id = 'run_live'`, sha("a"))
	f.taskRuns(13, "older", 40)
	f.exec(`UPDATE runs SET source_commit = ? WHERE id = 'run_older'`, sha("d"))
	f.exec(`UPDATE tasks SET source_snapshot = ? WHERE id = 'task_older'`, sha("d"))
	rel := f.relayWith(domain.SnapshotRef(sha("a")), domain.SnapshotRef(sha("b")), domain.SnapshotRef(sha("c")),
		domain.SnapshotRef(sha("d")), domain.SnapshotRef(sha("e")))

	if _, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	var snaps []string
	for _, r := range rel.deleted {
		if strings.HasPrefix(r, "refs/cf/snapshots/") {
			snaps = append(snaps, r)
		}
	}
	// a is shared with a live Run, b is inside the grace, c is a live Task's, e belongs to nobody known.
	if want := []string{domain.SnapshotRef(sha("d"))}; !slices.Equal(snaps, want) {
		t.Errorf("deleted snapshots %v, want %v", snaps, want)
	}
}
