//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The checker is judged on a cluster it never ran: a sound history, built by
// hand, with each invariant broken in turn.

var ids = domain.NewIDGenerator(time.Now, rand.Reader)

// world is a sound history of three Tasks on two Workers:
//   - A completed on w1;
//   - B lost on w1, retried on w2 where it completed; its orphan came back,
//     finished and was published as a lost Run is in Phase 1;
//   - C cancelled by the test while queued.
//
// The daemon of w1 was killed once, at high-water mark 3, and reported in its
// next epoch after it.
type world struct {
	obs Observation
	exp Expect

	a1, b1, b2, c1 string
}

func seq(n int64) *int64 { return &n }

func newWorld() *world {
	w := &world{
		a1: string(ids.New(domain.KindRun)), b1: string(ids.New(domain.KindRun)),
		b2: string(ids.New(domain.KindRun)), c1: string(ids.New(domain.KindRun)),
	}
	now := time.Now()
	zero := 0
	retry := v1.Retry{OnFailed: domain.RetryNone, OnLost: domain.RetryReschedule, MaxAttempts: 2}
	w.exp = Expect{
		Tasks:       []ExpectedTask{{ID: "task-a", Token: "t1"}, {ID: "task-b", Token: "t2"}, {ID: "task-c", Token: "t3", Cancelled: true}},
		DaemonKills: []DaemonKill{{Worker: "w1", HighWater: 3, Epochs: []string{"e1"}}},
	}
	w.obs = Observation{
		Tasks: []v1.Task{
			{ID: "task-a", State: domain.TaskCompleted, Retry: retry},
			{ID: "task-b", State: domain.TaskCompleted, Retry: retry},
			{ID: "task-c", State: domain.TaskCancelled, Retry: retry, CancelledAt: &now},
		},
		Runs: []v1.Run{
			{
				ID: w.a1, TaskID: "task-a", Attempt: 1, State: domain.RunCompleted, EndReason: domain.EndExitCode, ExitCode: &zero,
				Lineage: domain.LineageInitial, Branch: "cf/a1", ResultCommit: "c1", Publication: domain.PublicationPublished,
				Runtime: domain.RuntimeStopped, FinishedAt: &now,
			},
			{
				ID: w.b1, TaskID: "task-b", Attempt: 1, State: domain.RunLost, Lineage: domain.LineageInitial,
				Branch: "cf/b1", ResultCommit: "c2", Publication: domain.PublicationPublished, Runtime: domain.RuntimeStopped, FinishedAt: &now,
			},
			{
				ID: w.b2, TaskID: "task-b", Attempt: 2, State: domain.RunCompleted, EndReason: domain.EndExitCode, ExitCode: &zero,
				Lineage: domain.LineageRetries, LineageParent: w.b1, Branch: "cf/b2", ResultCommit: "c3",
				Publication: domain.PublicationPublished, Runtime: domain.RuntimeStopped, FinishedAt: &now,
			},
			{
				ID: w.c1, TaskID: "task-c", Attempt: 1, State: domain.RunCancelled, Lineage: domain.LineageInitial,
				Publication: domain.PublicationNone, Runtime: domain.RuntimeNone, FinishedAt: &now,
			},
		},
		Snapshot: ControllerSnapshot{
			Events: []EventRow{
				{Type: domain.EventTaskCreated}, // no Run, no triple
				{Type: domain.EventRunStarted, RunID: w.a1, WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: seq(1), ReportDigest: "d1", LeaseEpoch: 1},
				{Type: domain.EventRunStarted, RunID: w.b1, WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: seq(2), ReportDigest: "d2", LeaseEpoch: 1},
				{Type: domain.EventRunCompleted, RunID: w.a1, WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: seq(3), ReportDigest: "d3", LeaseEpoch: 1},
				{Type: domain.EventRunSealed, RunID: w.a1},
				{Type: domain.EventRunPublished, RunID: w.a1},
				{Type: domain.EventRunLost, RunID: w.b1}, // the Controller's: no triple
				{Type: domain.EventRunStarted, RunID: w.b2, WorkerID: "wk2", WorkerEpoch: "f1", WorkerSeq: seq(1), ReportDigest: "g1", LeaseEpoch: 1},
				{Type: domain.EventRunCompleted, RunID: w.b2, WorkerID: "wk2", WorkerEpoch: "f1", WorkerSeq: seq(2), ReportDigest: "g2", LeaseEpoch: 1},
				{Type: domain.EventRunSealed, RunID: w.b2},
				{Type: domain.EventRunPublished, RunID: w.b2},
				// the orphan, after the kill, in the next epoch of w1
				{Type: domain.EventRunExited, RunID: w.b1, WorkerID: "wk1", WorkerEpoch: "e2", WorkerSeq: seq(4), ReportDigest: "d4", LeaseEpoch: 1},
				{Type: domain.EventRunOrphaned, RunID: w.b1, LeaseEpoch: 1},
				{Type: domain.EventRunStopped, RunID: w.b1, WorkerID: "wk1", WorkerEpoch: "e2", WorkerSeq: seq(5), ReportDigest: "d5", LeaseEpoch: 1},
				{Type: domain.EventRunSealed, RunID: w.b1},
				{Type: domain.EventRunPublished, RunID: w.b1},
				{Type: domain.EventRunCancelled, RunID: w.c1},
			},
			Receipts: []ReceiptRow{
				{WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: 1, RunID: w.a1, Digest: "d1"},
				{WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: 2, RunID: w.b1, Digest: "d2"},
				{WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: 3, RunID: w.a1, Digest: "d3"},
				{WorkerID: "wk1", WorkerEpoch: "e2", WorkerSeq: 4, RunID: w.b1, Digest: "d4"},
				{WorkerID: "wk1", WorkerEpoch: "e2", WorkerSeq: 5, RunID: w.b1, Digest: "d5"},
				{WorkerID: "wk1", WorkerEpoch: "e2", WorkerSeq: 6, RunID: w.a1, Digest: "d6"}, // a receipt with no event
				{WorkerID: "wk2", WorkerEpoch: "f1", WorkerSeq: 1, RunID: w.b2, Digest: "g1"},
				{WorkerID: "wk2", WorkerEpoch: "f1", WorkerSeq: 2, RunID: w.b2, Digest: "g2"},
			},
			Leases: []LeaseRow{
				{ID: "l-a1", RunID: w.a1, WorkerID: "wk1", Epoch: 1, State: string(domain.LeaseAccepted)},
				{ID: "l-b1", RunID: w.b1, WorkerID: "wk1", Epoch: 1, State: string(domain.LeaseAccepted)},
				{ID: "l-b2", RunID: w.b2, WorkerID: "wk2", Epoch: 1, State: string(domain.LeaseAccepted)},
			},
			Orphans: []OrphanRow{{WorkerID: "wk1", RunID: w.b1, LeaseEpoch: 1, Preserved: "worker"}},
		},
		WorkerIDs: map[string]string{"w1": "wk1", "w2": "wk2"},
		Instances: []Instance{
			{Execution: Execution{Run: w.a1, Epoch: 1, Worker: "w1"}, Token: "t1", PID: 101, Ended: true, Verified: true},
			{Execution: Execution{Run: w.b1, Epoch: 1, Worker: "w1"}, Token: "t2", PID: 102, Ended: true, Verified: true},
			{Execution: Execution{Run: w.b2, Epoch: 1, Worker: "w2"}, Token: "t2", PID: 201, Ended: true, Verified: true},
		},
		RelayRefs: map[string]string{"refs/heads/cf/a1": "c1", "refs/heads/cf/b1": "c2", "refs/heads/cf/b2": "c3"},
		Results:   map[string]string{"c1": "t1\n", "c2": "t2\n", "c3": "t2\n"},
	}
	return w
}

func (w *world) run(id string) *v1.Run {
	i := slices.IndexFunc(w.obs.Runs, func(r v1.Run) bool { return r.ID == id })
	return &w.obs.Runs[i]
}

func (w *world) task(id string) *v1.Task {
	i := slices.IndexFunc(w.obs.Tasks, func(t v1.Task) bool { return t.ID == id })
	return &w.obs.Tasks[i]
}

func (w *world) event(e EventRow) { w.obs.Snapshot.Events = append(w.obs.Snapshot.Events, e) }

func (w *world) dropEvents(drop func(EventRow) bool) {
	w.obs.Snapshot.Events = slices.DeleteFunc(w.obs.Snapshot.Events, drop)
}

func TestInvariants_SoundHistories(t *testing.T) {
	cases := map[string]func(w *world){
		"as built": func(*world) {},
		"a cancelled Task whose Run ran has its result published": func(w *world) {
			w.run(w.c1).Publication, w.run(w.c1).Branch, w.run(w.c1).ResultCommit = domain.PublicationPublished, "cf/c1", "c4"
			w.obs.RelayRefs["refs/heads/cf/c1"] = "c4"
			w.obs.Snapshot.Leases = append(w.obs.Snapshot.Leases, LeaseRow{RunID: w.c1, WorkerID: "wk2", Epoch: 1, State: string(domain.LeaseAccepted)})
			w.obs.Instances = append(w.obs.Instances, Instance{Execution: Execution{Run: w.c1, Epoch: 1, Worker: "w2"}, Token: "t3", PID: 202})
			w.event(EventRow{Type: domain.EventRunStarted, RunID: w.c1, WorkerID: "wk2", WorkerEpoch: "f1", WorkerSeq: seq(3), ReportDigest: "g3"})
		},
		"a lost Run with no attempts left fails its Task": func(w *world) {
			w.task("task-b").Retry.MaxAttempts = 1
			w.task("task-b").State = domain.TaskFailed
			w.obs.Runs = slices.DeleteFunc(w.obs.Runs, func(r v1.Run) bool { return r.ID == w.b2 })
			w.obs.Instances = w.obs.Instances[:2]
		},
		"an orphan preserved in the relay": func(w *world) {
			w.obs.Snapshot.Orphans[0].Preserved, w.obs.Snapshot.Orphans[0].ResultCommit = "relay", "c9"
			w.obs.RelayRefs[domain.OrphanRef(domain.ID(w.b1), 1)] = "c9"
		},
		"two Runs share a result OID": func(w *world) {
			w.run(w.b2).ResultCommit = "c2"
			w.obs.RelayRefs["refs/heads/cf/b2"] = "c2"
		},
		"a harness stopped before it finished has no content to match": func(w *world) {
			w.obs.Instances[1].Ended = false
			w.obs.Results["c2"] = ""
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			change(w)
			if got := Violations(w.obs, w.exp); len(got) != 0 {
				t.Errorf("violations of a sound history:\n  %s", strings.Join(got, "\n  "))
			}
		})
	}
}

func TestInvariants_Violations(t *testing.T) {
	cases := []struct {
		name   string
		change func(w *world)
		want   string
	}{
		// quiet
		{"not quiet", func(w *world) { w.obs.Unquiet = "Task task-a is queued" }, "[quiet] Task task-a is queued"},

		// 1: no Run lost, every Task settled
		{"a Task the test did not create", func(w *world) {
			w.obs.Tasks = append(w.obs.Tasks, v1.Task{ID: "task-x", State: domain.TaskCompleted})
		}, "Task task-x exists, but the test created no such Task"},
		{"a Task of the test is missing", func(w *world) { w.obs.Tasks = w.obs.Tasks[:2] }, "Task task-c, which the test created, is missing"},
		{"a Task left queued", func(w *world) { w.task("task-a").State = domain.TaskQueued }, "Task task-a ended queued"},
		{"a Task waiting for a start", func(w *world) {
			w.task("task-a").State, w.task("task-a").WaitingReason = domain.TaskWaiting, domain.WaitingStart
		}, "Task task-a ended waiting(start)"},
		{"a Task cancelled that the test did not cancel", func(w *world) { w.exp.Tasks[2].Cancelled = false }, "Task task-c is cancelled, but the test did not cancel it"},
		{"a cancel that did not take", func(w *world) { w.task("task-c").State = domain.TaskWaiting }, "Task task-c was cancelled by the test, but is waiting"},
		{"a Task failed though its last Run completed", func(w *world) { w.task("task-a").State = domain.TaskFailed }, "Task task-a is failed, but its last Run"},
		{"a Run still running", func(w *world) { w.run(w.a1).State = domain.RunRunning }, "is still running"},
		{"a Run cancelled whose Task was not", func(w *world) { w.exp.Tasks[2].Cancelled = false }, "is cancelled, but the test did not cancel its Task task-c"},
		{"a completed Run that exited 1", func(w *world) { one := 1; w.run(w.a1).ExitCode = &one }, "is completed with end_reason \"exit_code\" and exit code 1"},
		{"a failed Run without a cause", func(w *world) {
			w.run(w.a1).State, w.run(w.a1).EndReason = domain.RunFailed, ""
		}, "failed without an end_reason"},
		{"a settled Run without finished_at", func(w *world) { w.run(w.b1).FinishedAt = nil }, "is lost without finished_at"},
		{"a lost Run whose retry is missing", func(w *world) {
			w.obs.Runs = slices.DeleteFunc(w.obs.Runs, func(r v1.Run) bool { return r.ID == w.b2 })
		}, "is lost with 1 attempts left, but has no retry"},
		{"a retry the policy does not ask for", func(w *world) { w.task("task-b").Retry.OnLost = domain.RetryAsk }, "which the policy of Task task-b does not retry"},
		{"a Run retried twice", func(w *world) {
			r := *w.run(w.b2)
			r.ID, r.Attempt, r.State, r.Publication = string(ids.New(domain.KindRun)), 3, domain.RunCancelled, domain.PublicationNone
			w.obs.Runs = append(w.obs.Runs, r)
		}, "was retried 2 times"},

		// 2: no duplicates
		{"two RunStarted", func(w *world) { w.event(EventRow{Type: domain.EventRunStarted, RunID: w.a1}) }, "has 2 RunStarted"},
		{"no terminal event", func(w *world) {
			w.dropEvents(func(e EventRow) bool { return e.RunID == w.c1 && e.Type == domain.EventRunCancelled })
		}, "(cancelled) has 0 terminal events"},
		{"two terminal events", func(w *world) { w.event(EventRow{Type: domain.EventRunLost, RunID: w.a1}) }, "(completed) has 2 terminal events"},
		{"a terminal event of another state", func(w *world) {
			w.dropEvents(func(e EventRow) bool { return e.RunID == w.c1 })
			w.event(EventRow{Type: domain.EventRunFailed, RunID: w.c1})
		}, "is cancelled, but it ended with RunFailed"},
		{"two accepted leases", func(w *world) {
			w.obs.Snapshot.Leases = append(w.obs.Snapshot.Leases, LeaseRow{RunID: w.a1, WorkerID: "wk2", Epoch: 2, State: string(domain.LeaseAccepted)})
		}, "has 2 accepted leases"},
		{"two completed Runs of a Task", func(w *world) {
			zero := 0
			w.run(w.b1).State, w.run(w.b1).EndReason, w.run(w.b1).ExitCode = domain.RunCompleted, domain.EndExitCode, &zero
		}, "Task task-b has 2 completed Runs"},

		// 3: one harness per Run
		{"a harness in no Worker's worktree", func(w *world) { w.obs.Instances[0].Worker = "" }, "the worktree of no Worker"},
		{"a harness of another Task", func(w *world) { w.obs.Instances[0].Token = "t2" }, "the harness of t2 ran"},
		{"a harness without its lease", func(w *world) { w.obs.Instances[0].Epoch = 2 }, "ran without an accepted lease of that Worker"},
		{"a harness on another Worker than its lease", func(w *world) { w.obs.Instances[0].Worker = "w2" }, "ran without an accepted lease of that Worker"},
		{"two harnesses of a Run", func(w *world) {
			w.obs.Instances = append(w.obs.Instances, Instance{Execution: Execution{Run: w.a1, Epoch: 1, Worker: "w1"}, Token: "t1", PID: 103})
		}, "had 2 harnesses"},
		{"the sampler saw two alive", func(w *world) { w.obs.Sampled = []string{"[3 one harness] at 12:00 two alive"} }, "at 12:00 two alive"},

		// 4: publication
		{"a harness that ran, unpublished", func(w *world) { w.run(w.b1).Publication = domain.PublicationFailed }, "ran its harness, but its publication is failed"},
		{"published without a commit", func(w *world) { w.run(w.a1).ResultCommit = "" }, "is published without a result_commit"},
		{"a branch whose tip is not the result", func(w *world) { w.obs.RelayRefs["refs/heads/cf/a1"] = "c3" }, "but refs/heads/cf/a1 is \"c3\""},
		{"a branch missing", func(w *world) { delete(w.obs.RelayRefs, "refs/heads/cf/b2") }, "but refs/heads/cf/b2 is \"\""},
		{"a result of another Task", func(w *world) { w.obs.Results["c1"] = "t2\n" }, "with result.txt \"t2\", want \"t1\""},
		{"a result without result.txt", func(w *world) { delete(w.obs.Results, "c3") }, "with result.txt \"\", want \"t2\""},
		{"an orphan preserved but not in the relay", func(w *world) {
			w.obs.Snapshot.Orphans[0].Preserved, w.obs.Snapshot.Orphans[0].ResultCommit = "relay", "c9"
		}, "is c9, but the relay has \"\""},
		{"an orphan ref nobody preserved", func(w *world) {
			w.obs.RelayRefs[domain.OrphanRef(domain.ID(w.b1), 1)] = "c9"
		}, "an orphan the Controller did not preserve"},

		// 5: journal
		{"two events of one entry", func(w *world) {
			w.event(EventRow{Type: domain.EventRunStopped, RunID: w.a1, WorkerID: "wk1", WorkerEpoch: "e1", WorkerSeq: seq(1), ReportDigest: "d1"})
		}, "[5a journal] {wk1 e1 1} has 2 events"},
		{"two receipts of one entry", func(w *world) {
			w.obs.Snapshot.Receipts = append(w.obs.Snapshot.Receipts, w.obs.Snapshot.Receipts[0])
		}, "[5a journal] {wk1 e1 1} has 2 receipts"},
		{"an event and a receipt of different digests", func(w *world) { w.obs.Snapshot.Receipts[2].Digest = "dx" }, "digest \"d3\", the receipt Run"},
		{"an event and a receipt of different Runs", func(w *world) { w.obs.Snapshot.Receipts[0].RunID = w.b1 }, "{wk1 e1 1}: the event says Run"},
		{"a seq in two epochs", func(w *world) {
			w.obs.Snapshot.Receipts = append(w.obs.Snapshot.Receipts, ReceiptRow{WorkerID: "wk2", WorkerEpoch: "f2", WorkerSeq: 1, RunID: w.b2, Digest: "gx"})
		}, "[5b journal] seq 1 of Worker wk2 is in epochs [f1 f2]"},
		{"a new epoch reusing a seq under the mark", func(w *world) {
			w.obs.Snapshot.Receipts = append(w.obs.Snapshot.Receipts, ReceiptRow{WorkerID: "wk1", WorkerEpoch: "e3", WorkerSeq: 2, RunID: w.a1, Digest: "dy"})
		}, "w1 reported seq 2 in the new epoch e3, not above the high-water mark 3"},
		{"only the replay after a kill", func(w *world) {
			w.dropEvents(func(e EventRow) bool { return e.WorkerEpoch == "e2" })
			w.obs.Snapshot.Receipts = slices.DeleteFunc(w.obs.Snapshot.Receipts, func(r ReceiptRow) bool { return r.WorkerEpoch == "e2" })
		}, "w1 reported nothing in a new epoch after its kill at high-water mark 3"},
		{"a kill of no Worker", func(w *world) { w.exp.DaemonKills[0].Worker = "w9" }, "the killed daemon of w9 is of no Worker"},

		// 6: reservations
		{"a reservation open", func(w *world) {
			w.obs.Snapshot.OpenReservations = []ReservationRow{{LeaseID: "l-b1", RunID: w.b1, WorkerID: "wk1"}}
		}, "[6 reservations] the reservation of Run"},

		// 7: nothing left
		{"a harness alive when quiet", func(w *world) { w.obs.Live = []Instance{w.obs.Instances[1]} }, "is alive once the cluster is quiet"},

		// 8: once per Run
		{"two RunSealed", func(w *world) { w.event(EventRow{Type: domain.EventRunSealed, RunID: w.a1}) }, "has 2 RunSealed"},
		{"two RunPublished", func(w *world) { w.event(EventRow{Type: domain.EventRunPublished, RunID: w.a1}) }, "has 2 RunPublished"},
		{"two RunStopped", func(w *world) { w.event(EventRow{Type: domain.EventRunStopped, RunID: w.b1}) }, "has 2 RunStopped"},
		{"two RunOrphaned of one epoch", func(w *world) {
			w.event(EventRow{Type: domain.EventRunOrphaned, RunID: w.b1, LeaseEpoch: 1})
		}, "has 2 RunOrphaned under lease epoch 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld()
			tc.change(w)
			got := Violations(w.obs, w.exp)
			if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, tc.want) }) {
				t.Errorf("want a violation with %q, got:\n  %s", tc.want, strings.Join(got, "\n  "))
			}
		})
	}
}

// TestInvariants_AllGathered: one history that breaks many invariants reports
// every one, not the first.
func TestInvariants_AllGathered(t *testing.T) {
	w := newWorld()
	w.obs.Unquiet = "still moving"
	w.obs.Snapshot.OpenReservations = []ReservationRow{{RunID: w.a1}}
	w.event(EventRow{Type: domain.EventRunSealed, RunID: w.a1})
	got := Violations(w.obs, w.exp)
	for _, tag := range []string{"[quiet]", "[6 reservations]", "[8 once]"} {
		if !slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, tag) }) {
			t.Errorf("no %s in %v", tag, got)
		}
	}
}

// TestLedger_ReleaseExecution: releasing one execution ends its harness and
// no other of its Task; releasing the token ends the rest.
func TestLedger_ReleaseExecution(t *testing.T) {
	data := t.TempDir()
	box := &Box{Paths: config.Paths{DataDir: data}}
	l := newLedger(t, t.TempDir(), []*Worker{{Name: "w1", Box: box}})
	token := l.Token()
	run := string(ids.New(domain.KindRun))
	start := func(epoch uint64) chan error {
		dir := box.Paths.WorkerWorktreeDir(run, epoch)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "sh", "-c", l.Script(token)) //nolint:gosec // the ledger's own script
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return done
	}
	ended := func(done chan error) bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the script: %v", err)
			}
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	first, second := start(1), start(2)
	waitFor(t, func() bool { return len(l.Instances(token)) == 2 })

	l.ReleaseExecution(Execution{Run: run, Epoch: 1, Worker: "w1"})
	if !ended(first) {
		t.Fatal("the released execution did not end")
	}
	select {
	case <-second:
		t.Fatal("an execution that was not released ended")
	case <-time.After(3 * ledgerPoll):
	}
	if got, err := os.ReadFile(filepath.Join(box.Paths.WorkerWorktreeDir(run, 1), "result.txt")); err != nil || strings.TrimSpace(string(got)) != token {
		t.Errorf("result.txt = %q, %v", got, err)
	}

	l.Release(token)
	if !ended(second) {
		t.Fatal("the token did not release the other execution")
	}
	got := l.Instances(token)
	want := []Execution{{Run: run, Epoch: 1, Worker: "w1"}, {Run: run, Epoch: 2, Worker: "w1"}}
	for _, in := range got {
		if !in.Ended || !slices.Contains(want, in.Execution) {
			t.Errorf("instance %+v", in)
		}
	}
	if len(got) != 2 {
		t.Errorf("%d instances, want 2", len(got))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatal("e2e: timed out")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
