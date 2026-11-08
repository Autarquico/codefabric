package gc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

var epoch = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

type fixture struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	blobs *blob.Store
	root  string
	runs  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(epoch)
	root := t.TempDir()
	blobs, err := blob.New(filepath.Join(root, "data"), clk)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(root, "controller.db"), store.Options{Clock: clk, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{t: t, db: db, clk: clk, blobs: blobs, root: filepath.Join(root, "data")}
	f.tx(func(tx *store.Tx) error {
		p := store.Project{ID: "project_1", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: epoch}
		if err := tx.InsertProject(t.Context(), p); err != nil {
			return err
		}
		return tx.InsertTask(t.Context(), store.Task{
			ID: "task_1", Short: 1, ProjectID: p.ID, Name: "t", Prompt: "p",
			State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: epoch,
		})
	})
	return f
}

func (f *fixture) service(p gc.Policy, opts ...gc.Option) *gc.Service {
	return gc.New(f.db, f.clk, f.blobs, p, opts...)
}

func (f *fixture) tx(fn func(*store.Tx) error) {
	f.t.Helper()
	if err := f.db.WithTx(f.t.Context(), fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	f.tx(func(tx *store.Tx) error { _, err := tx.Exec(f.t.Context(), query, args...); return err })
}

func (f *fixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.db.Reader().QueryRowContext(f.t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// run inserts a Run; ended Runs are failed, the others queued.
func (f *fixture) run(ended bool) domain.ID {
	f.t.Helper()
	f.runs++
	id := domain.ID(fmt.Sprintf("run_%c", 'a'+f.runs))
	f.tx(func(tx *store.Tx) error {
		return tx.InsertRun(f.t.Context(), store.Run{
			ID: id, Short: uint64(f.runs), TaskID: "task_1", Attempt: f.runs, Lineage: domain.LineageInitial, //nolint:gosec // small counter
			Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch,
		})
	})
	if ended {
		f.exec(`UPDATE runs SET state = 'failed', finished_at = ? WHERE id = ?`, epoch.UnixNano(), string(id))
	}
	return id
}

// event appends an event received now, for the Run if given.
func (f *fixture) event(run domain.ID) uint64 {
	f.t.Helper()
	var seq uint64
	f.tx(func(tx *store.Tx) (err error) {
		e := domain.Event{Type: domain.EventRunQueued, Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}, RunID: run}
		seq, _, err = tx.AppendEvent(f.t.Context(), e)
		return err
	})
	return seq
}

func digestOf(content string) blob.Digest {
	sum := sha256.Sum256([]byte(content))
	return blob.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// blob stores content and sets its mtime to age before the clock.
func (f *fixture) blob(content string, age time.Duration) blob.Digest {
	f.t.Helper()
	d, _, err := f.blobs.Put(f.t.Context(), bytes.NewReader([]byte(content)), "")
	if err != nil {
		f.t.Fatal(err)
	}
	f.touch(d, f.clk.Now().Add(-age))
	return d
}

func (f *fixture) touch(d blob.Digest, at time.Time) {
	f.t.Helper()
	h := strings.TrimPrefix(string(d), "sha256:")
	if err := os.Chtimes(filepath.Join(f.root, "blobs", "sha256", h[0:2], h[2:4], h), at, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) has(d blob.Digest) bool {
	f.t.Helper()
	ok, err := f.blobs.Has(d)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

// segment indexes d as the only segment of the output of run, last touched at updated.
func (f *fixture) segment(run domain.ID, d blob.Digest, updated time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO run_output (run_id, lease_epoch, start_offset, end_offset, sealed_offset, accepted_bytes, updated_at) VALUES (?, 1, 0, 10, 10, 10, ?)`,
		string(run), updated.UnixNano())
	f.exec(`INSERT INTO logs_index (run_id, lease_epoch, seg_no, first_offset, last_offset, raw_bytes, digest) VALUES (?, 1, 0, 0, 10, 10, ?)`,
		string(run), string(d))
}

func (f *fixture) upload(d blob.Digest, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO blobs_meta (digest, size, created_at) VALUES (?, 1, ?)`, string(d), at.UnixNano())
}

func items(rep v1.GCReport) map[string]int64 {
	out := map[string]int64{}
	for _, it := range rep.Items {
		out[it.Kind] = it.Count
	}
	return out
}

var shortPolicy = gc.Policy{
	System: 2 * day, Events: 2 * day, RunLogs: 2 * day, RawOutput: 2 * day, BlobGrace: day, BatchSize: 50,
}

func TestScenario_RetentionGc_DryRun(t *testing.T) {
	f := newFixture(t)
	for range 5 {
		f.event("")
	}
	f.clk.Advance(10 * day)
	f.event("") // recent: stays
	f.event("")
	orphan := f.blob("nobody wants this", 5*day)
	f.exec(`INSERT INTO logs (ts, received_at, source, level, msg) VALUES (1, ?, 'controller', 'info', 'old')`, epoch.UnixNano())
	svc := f.service(shortPolicy)

	dry, err := svc.Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || items(dry)["events"] != 5 || items(dry)["system_logs"] != 1 || items(dry)["blobs"] != 1 {
		t.Fatalf("dry run = %+v", dry)
	}
	if f.count("events") != 7 || f.count("logs") != 1 || !f.has(orphan) {
		t.Fatal("a dry run changed something")
	}

	applied, err := svc.Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if applied.DryRun || !slices.Equal(applied.Items, dry.Items) {
		t.Fatalf("the pass deleted %+v, the dry run listed %+v", applied.Items, dry.Items)
	}
	if f.count("events") != 2 || f.count("logs") != 0 || f.has(orphan) {
		t.Fatalf("after the pass: %d events, %d logs, orphan present %v", f.count("events"), f.count("logs"), f.has(orphan))
	}
}

func TestScenario_RetentionGc_ReferenciasVivas(t *testing.T) {
	f := newFixture(t)
	f.clk.Advance(30 * day)
	old := 20 * day

	live := f.run(false)
	liveSegment := f.blob("segment of a live run", old)
	f.segment(live, liveSegment, f.clk.Now().Add(-old)) // untouched for longer than raw_output, but the Run has not ended

	recentEnded := f.run(true)
	recentSegment := f.blob("segment of a recent run", old)
	f.segment(recentEnded, recentSegment, f.clk.Now().Add(-time.Hour))

	manifestFile := f.blob("a file of a workflow", old)
	f.exec(`INSERT INTO workflows (id, name, created_by, created_at) VALUES ('wf_1', 'w', 'me', 1)`)
	f.exec(`INSERT INTO workflow_versions (workflow_id, version, digest, manifest, author_kind, author_id, origin, changed, created_at)
		VALUES ('wf_1', 1, ?, ?, 'user', 'me', 'client', '[]', 1)`, string(digestOf("manifest")),
		fmt.Sprintf(`{"schema_version":1,"components":[{"files":[{"sha256":%q}]}]}`, manifestFile))
	f.upload(manifestFile, f.clk.Now().Add(-old))

	justUploaded := f.blob("uploaded and not committed yet", time.Hour)
	f.upload(justUploaded, f.clk.Now().Add(-time.Hour))

	staleUpload := f.blob("uploaded and forgotten", old)
	f.upload(staleUpload, f.clk.Now().Add(-old))
	orphan := f.blob("orphan segment of an aborted seal", old)

	endedLongAgo := f.run(true)
	purgedSegment := f.blob("segment of an old run", old)
	f.segment(endedLongAgo, purgedSegment, f.clk.Now().Add(-old))

	svc := f.service(shortPolicy)
	dry, err := svc.Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Items, dry.Items) {
		t.Fatalf("the dry run said %+v and the pass did %+v", dry.Items, rep.Items)
	}

	for name, d := range map[string]blob.Digest{
		"segment of a live Run": liveSegment, "segment of a recent Run": recentSegment,
		"file named by a manifest": manifestFile, "recent upload": justUploaded,
	} {
		if !f.has(d) {
			t.Errorf("%s was deleted", name)
		}
	}
	for name, d := range map[string]blob.Digest{
		"forgotten upload": staleUpload, "orphan segment": orphan, "segment of an expired output": purgedSegment,
	} {
		if f.has(d) {
			t.Errorf("%s is still there", name)
		}
	}
	if f.count("logs_index") != 2 {
		t.Errorf("%d segments indexed, want the two of Runs whose output is not expired", f.count("logs_index"))
	}
	if f.count("blobs_meta") != 2 {
		t.Errorf("%d uploads recorded, want the committed file and the recent one", f.count("blobs_meta"))
	}
}

func TestScenario_RetentionGc_PurgedOutputIsReadableAsTruncated(t *testing.T) {
	f := newFixture(t)
	f.clk.Advance(30 * day)
	run := f.run(true)
	f.segment(run, f.blob("old output", 20*day), f.clk.Now().Add(-20*day))

	if _, err := f.service(shortPolicy).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	var o store.Output
	if err := f.db.Read(t.Context(), func(q store.Querier) (err error) {
		o, err = store.OutputOf(t.Context(), q, run, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if o.Start != o.End || o.End != 10 {
		t.Fatalf("output = [%d, %d), want it emptied at its end", o.Start, o.End)
	}
}

func TestScenario_RetentionGc_EventsOfARunThatHasNotEndedAreKept(t *testing.T) {
	f := newFixture(t)
	live := f.run(false)
	first := f.event("")
	second := f.event(live)
	f.event("")
	f.event("")
	f.clk.Advance(10 * day)
	f.event("")

	rep, err := f.service(shortPolicy).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if items(rep)["events"] != 1 {
		t.Fatalf("purged %d events, want only the one before the live Run's", items(rep)["events"])
	}
	var purged uint64
	if err := f.db.Read(t.Context(), func(q store.Querier) (err error) {
		_, purged, err = store.EventBounds(t.Context(), q)
		return err
	}); err != nil || purged != first {
		t.Fatalf("events_purged_through = %d, %v; want %d, not %d", purged, err, first, second)
	}
}

func TestScenario_RetentionGc_Latencia(t *testing.T) {
	const batch = 10
	f := newFixture(t)
	for range 95 {
		f.event("")
		f.exec(`INSERT INTO logs (ts, received_at, source, level, msg) VALUES (1, ?, 'controller', 'info', 'x')`, epoch.UnixNano())
	}
	f.clk.Advance(10 * day)
	f.event("")
	var orphans []blob.Digest
	for i := range 35 {
		orphans = append(orphans, f.blob(fmt.Sprintf("orphan %d", i), 5*day))
	}

	var batches, writes int
	hooks := gc.Hooks{AfterBatch: func(kind string, rows int64) {
		batches++
		if rows > batch {
			t.Errorf("a batch of %s touched %d rows, the limit is %d", kind, rows, batch)
		}
		// The GC holds no transaction here: a write goes through at once. A writer
		// that waited for the whole pass would hit this deadline.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := f.db.WithTx(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO controller_meta (key, value) VALUES (?, 'x') ON CONFLICT (key) DO NOTHING`, fmt.Sprintf("probe-%d", batches))
			return err
		}); err != nil {
			t.Errorf("a write between batches failed: %v", err)
			return
		}
		writes++
	}}
	policy := shortPolicy
	policy.BatchSize = batch
	rep, err := f.service(policy, gc.WithHooks(hooks)).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if items(rep)["events"] != 95 || items(rep)["system_logs"] != 95 || items(rep)["blobs"] != 35 {
		t.Fatalf("report = %+v", rep.Items)
	}
	if writes != batches || batches < 95/batch+95/batch+35/batch {
		t.Fatalf("%d writes in %d batches", writes, batches)
	}
	for _, d := range orphans {
		if f.has(d) {
			t.Fatal("an orphan survived")
		}
	}
}

// A writer that stores the blob again after the sweep has decided to delete it,
// and before it unlinks it, must keep it.
func TestScenario_RetentionGc_PutDuringSweepKeepsTheBlob(t *testing.T) {
	f := newFixture(t)
	const content = "uploaded again at the worst moment"
	d := f.blob(content, 5*day)
	f.upload(d, f.clk.Now().Add(-5*day))
	f.blob("a plain orphan", 5*day)

	hooks := gc.Hooks{BeforeUnlink: func(got blob.Digest) {
		if got == d {
			if _, _, err := f.blobs.Put(t.Context(), strings.NewReader(content), ""); err != nil {
				t.Error(err)
			}
		}
	}}
	rep, err := f.service(shortPolicy, gc.WithHooks(hooks)).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !f.has(d) {
		t.Fatal("the blob that a writer put during the sweep was deleted")
	}
	if f.count("blobs_meta") != 1 {
		t.Fatal("its upload record was forgotten although the file stayed")
	}
	if items(rep)["blobs"] != 1 {
		t.Fatalf("deleted %d blobs, want only the plain orphan", items(rep)["blobs"])
	}
}

func TestScenario_RetentionGc_StorageStats(t *testing.T) {
	f := newFixture(t)
	f.clk.Advance(30 * day)
	run := f.run(false)
	segment := f.blob("output bytes!", 20*day) // 13 bytes
	f.segment(run, segment, f.clk.Now().Add(-time.Hour))
	file := f.blob("wf file", 20*day) // 7 bytes
	f.exec(`INSERT INTO workflows (id, name, created_by, created_at) VALUES ('wf_1', 'w', 'me', 1)`)
	f.exec(`INSERT INTO workflow_versions (workflow_id, version, digest, manifest, author_kind, author_id, origin, changed, created_at)
		VALUES ('wf_1', 1, ?, ?, 'user', 'me', 'client', '[]', 1)`, string(digestOf("m")),
		fmt.Sprintf(`{"schema_version":1,"components":[{"files":[{"sha256":%q}]}]}`, file))
	orphan := f.blob("orphan!!", 20*day) // 8 bytes
	for range 3 {
		f.event("")
	}
	f.clk.Advance(10 * day)
	f.event("")
	f.blob("fresh", time.Minute) // 5 bytes, unreferenced but inside the grace period
	relay := filepath.Join(f.root, "relay")
	if err := os.MkdirAll(relay, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relay, "pack"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := f.service(shortPolicy, gc.WithDisk(filepath.Join(filepath.Dir(f.root), "controller.db"), relay))
	before := f.count("events")
	st, err := svc.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.OutputBlobBytes != 13 || st.WorkflowBlobBytes != 7 || st.UnreferencedBlobBytes != 13 || st.RelayBytes != 100 || st.DatabaseBytes == 0 {
		t.Fatalf("stats = %+v", st)
	}
	if st.TotalBytes != 13+7+13+100+st.DatabaseBytes {
		t.Fatalf("total = %d", st.TotalBytes)
	}
	if n := items(st.Reclaimable)["blobs"]; n != 1 {
		t.Fatalf("reclaimable blobs = %d, want the orphan only (the other is in its grace period)", n)
	}
	if bytes := st.Reclaimable.Items[len(st.Reclaimable.Items)-1].Bytes; bytes != 8 {
		t.Fatalf("reclaimable bytes = %d, want 8", bytes)
	}
	if items(st.Reclaimable)["events"] != 3 {
		t.Fatalf("reclaimable events = %d", items(st.Reclaimable)["events"])
	}
	if f.count("events") != before || !f.has(orphan) {
		t.Fatal("measuring changed something")
	}
}

func TestStats_FlagsStorageOverMaxStorage(t *testing.T) {
	f := newFixture(t)
	f.blob("0123456789", 5*day)
	policy := shortPolicy
	policy.MaxStorage = 5
	st, err := f.service(policy).Stats(t.Context())
	if err != nil || !st.OverMaxStorage || st.MaxStorageBytes != 5 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	if f.count("events") != 0 || len(st.Reclaimable.Items) == 0 {
		t.Fatal("unexpected state")
	}
}
