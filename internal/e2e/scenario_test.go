//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/e2e/netproxy"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// The scenarios of the spec e2e-v0-1 (§42.4 #1, #2, #3, #11 and #14 and the
// Done of Fase 1). Each waits for facts the cluster shows, never for a while,
// and ends with Check: every invariant holds once the cluster is quiet.

// TestScenario_E2eV01_DoneDeFase1 (#1, #2): two `cf do` on two Workers run one
// on each, and a third client sees them, their output and their events; once
// every process of that client is killed, the Runs finish and publish.
func TestScenario_E2eV01_DoneDeFase1(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 2, MaxConcurrent: 1})
	sampler := c.StartSampler()
	tokens := []string{c.Ledger.Token(), c.Ledger.Token()}
	var created []v1.CreateTaskResponse
	for _, token := range tokens {
		created = append(created, c.Do(c.Ledger.Script(token)))
	}

	// Each runs on a Worker of its own, the one the Controller says.
	harnesses := map[string]Instance{}
	for i, token := range tokens {
		in := waitHarness(c, token)
		run := c.WaitRun(created[i].Run.ID, "running", func(r v1.Run) bool { return r.State == domain.RunRunning })
		if want := (Execution{Run: run.ID, Epoch: 1, Worker: workerByID(c, run.WorkerID).Name}); in.Execution != want {
			t.Fatalf("the harness of %s ran as %v, the Controller says %v", run.ID, in.Execution, want)
		}
		harnesses[in.Worker] = in
	}
	if len(harnesses) != 2 {
		t.Fatalf("both Runs ran on one Worker: %v", harnesses)
	}

	// The third client sees both Runs, the output of one and its events.
	run := created[0].Run.ID
	ps := c.Ps(c.Observer)
	for _, w := range c.Workers {
		in := harnesses[w.Name]
		if !slices.ContainsFunc(ps, func(e v1.PsEntry) bool {
			return e.ID == in.Run && e.State == domain.RunRunning && e.Worker != nil && e.Worker.Name == w.Name
		}) {
			t.Fatalf("cf ps does not show %v running: %+v", in.Execution, ps)
		}
	}
	logs := c.FollowLogs(c.Observer, run)
	events := c.FollowEvents(c.Observer, "--run", run)
	c.Eventually("the output of the Run to reach the observer", func() (bool, string) {
		return strings.Contains(logs.Log(), "e2e: started "+tokens[0]), logs.Log()
	})
	c.Eventually("the events of the Run to reach the observer", func() (bool, string) {
		got := FollowedEvents(events)
		return slices.ContainsFunc(got, func(e v1.Event) bool {
			return e.RunID == run && e.Type == domain.EventRunStarted
		}), fmt.Sprintf("%d events", len(got))
	})
	for _, e := range FollowedEvents(events) {
		if e.RunID != run {
			t.Errorf("events --run %s gave an event of %q: %+v", run, e.RunID, e)
		}
	}

	// The observer's machine closes: every process of it dies without a word.
	for _, p := range []*Proc{logs, events} {
		p.Kill9()
	}
	if alive := runningIn(c.H, c.Observer); len(alive) > 0 {
		t.Fatalf("the observer still runs %v", alive)
	}
	for _, in := range harnesses {
		if !c.Ledger.Live(in) {
			t.Fatalf("the harness of %v died with the observer", in.Execution)
		}
	}

	// Nobody watches: the Runs finish and publish where the relay says.
	for _, token := range tokens {
		c.Ledger.Release(token)
	}
	for i, cr := range created {
		r := waitPublished(c, cr.Run.ID)
		if r.State != domain.RunCompleted {
			t.Errorf("Run %s = %s", r.ID, r.State)
		}
		if tip := gittest.Run(t, c.RelayRepo(), "rev-parse", "refs/heads/"+r.Branch); tip != r.ResultCommit {
			t.Errorf("relay tip of %s is %s, want the result %s", r.Branch, tip, r.ResultCommit)
		}
		if got := gittest.Run(t, c.RelayRepo(), "show", r.ResultCommit+":result.txt"); got != tokens[i] {
			t.Errorf("result.txt of %s = %q, want %q", r.ID, got, tokens[i])
		}
	}
	// What the dead follow missed is all there for whoever asks later.
	if out := c.mustCF(c.Observer, "logs", "--run", run).Stdout; !strings.Contains(out, "e2e: finished "+tokens[0]) {
		t.Errorf("the observer's logs of %s lack the end:\n%s", run, out)
	}

	Check(t, c, Expect{Tasks: expectedTasks(created, tokens), Sampler: sampler})
}

// TestScenario_E2eV01_CrashDelController (#3): kill -9 of the Controller with
// two Runs running and one queued loses no Run and runs none twice: the
// harnesses live on, the Workers reconcile, the queued one is assigned after
// that, and each Run starts, completes, seals and publishes exactly once.
func TestScenario_E2eV01_CrashDelController(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 2, MaxConcurrent: 1})
	sampler := c.StartSampler()
	tokens := []string{c.Ledger.Token(), c.Ledger.Token(), c.Ledger.Token()}
	var created []v1.CreateTaskResponse
	running := map[string]Instance{}
	for _, token := range tokens[:2] {
		created = append(created, c.Do(c.Ledger.Script(token)))
		running[token] = waitHarness(c, token)
	}
	created = append(created, c.Do(c.Ledger.Script(tokens[2])))
	third := created[2].Run.ID
	c.WaitRun(third, "queued for room", func(r v1.Run) bool { return r.State == domain.RunQueued && r.QueuedReason != "" })
	idents := identities(t, slices.Collect(maps.Values(running)))
	mark := lastEventSequence(c.ControllerSnapshot())

	c.Controller.Kill9()
	for _, in := range running {
		if procident.Verify(in.PID, idents[in.PID]) != procident.Same {
			t.Fatalf("the harness of %v died with the Controller", in.Execution)
		}
	}
	for _, w := range c.Workers {
		if !w.Daemon.Running() {
			t.Fatalf("the daemon of %s died with the Controller", w.Name)
		}
	}

	c.RestartController()
	var reconciled int64
	for _, w := range c.Workers {
		ev := waitEvent(c, "Worker "+w.Name+" to reconcile with the new Controller", func(e EventRow) bool {
			return e.Type == domain.EventWorkerReconciled && e.WorkerID == w.ID && e.Sequence > mark
		})
		reconciled = max(reconciled, ev.Sequence)
	}
	// The Runs went on with the harnesses they had, and the third still waits.
	for token, in := range running {
		if r := c.Run(c.Ctl, in.Run); r.State != domain.RunRunning || r.Runtime != domain.RuntimeActive {
			t.Fatalf("Run %s did not go on: %s", in.Run, RunStage(r))
		}
		if got := c.Ledger.Instances(token); len(got) != 1 || procident.Verify(in.PID, idents[in.PID]) != procident.Same {
			t.Fatalf("the harness of %v is not the one from before the crash: %+v", in.Execution, got)
		}
	}
	if r := c.Run(c.Ctl, third); r.State != domain.RunQueued {
		t.Fatalf("the third Run left the queue without room: %s", RunStage(r))
	}

	for _, token := range tokens[:2] {
		c.Ledger.Release(token)
	}
	waitHarness(c, tokens[2])
	c.Ledger.Release(tokens[2])
	snap := c.ControllerSnapshot()
	if assigned := eventsOf(snap, third, domain.EventRunAssigned); len(assigned) != 1 || assigned[0].Sequence < reconciled {
		t.Errorf("RunAssigned of the third %+v, the last WorkerReconciled after the restart is %d", assigned, reconciled)
	}
	for _, cr := range created {
		if r := waitPublished(c, cr.Run.ID); r.State != domain.RunCompleted {
			t.Errorf("Run %s = %s", r.ID, r.State)
		}
	}
	snap = c.ControllerSnapshot()
	for _, cr := range created {
		for _, typ := range []domain.EventType{domain.EventRunStarted, domain.EventRunCompleted, domain.EventRunSealed, domain.EventRunPublished} {
			if n := len(eventsOf(snap, cr.Run.ID, typ)); n != 1 {
				t.Errorf("%s: %d %s events", cr.Run.ID, n, typ)
			}
		}
		if n := countFunc(snap.Leases, func(l LeaseRow) bool { return l.RunID == cr.Run.ID }); n != 1 {
			t.Errorf("%s: %d leases", cr.Run.ID, n)
		}
	}

	Check(t, c, Expect{Tasks: expectedTasks(created, tokens), Sampler: sampler})
}

// TestScenario_E2eV01_CrashDelDaemon (#14): kill -9 of the daemon of the
// Worker while its harness runs loses nothing and runs nothing twice: the
// harness is the same process before and after, the Worker reconciles with a
// new epoch, reports something new under it, and the journal reuses no seq.
// The harness ends while no daemon looks, or after the new one took over.
func TestScenario_E2eV01_CrashDelDaemon(t *testing.T) {
	t.Run("release, then restart", func(t *testing.T) { crashDaemon(t, true) })
	t.Run("restart, then release", func(t *testing.T) { crashDaemon(t, false) })
}

func crashDaemon(t *testing.T, releaseFirst bool) {
	c := NewCluster(t, Opts{Workers: 1})
	w := c.Workers[0]
	sampler := c.StartSampler()
	token := c.Ledger.Token()
	created := c.Do(c.Ledger.Script(token))
	in := waitHarness(c, token)
	ident := identities(t, []Instance{in})[in.PID]

	kill := c.KillDaemon(w)
	mark := lastEventSequence(c.ControllerSnapshot())
	if procident.Verify(in.PID, ident) != procident.Same {
		t.Fatalf("the harness of %v died with the daemon", in.Execution)
	}
	if releaseFirst {
		c.Ledger.ReleaseExecution(in.Execution)
		c.Eventually("the harness to end with no daemon", func() (bool, string) {
			got := c.Ledger.Instances(token)
			return len(got) == 1 && got[0].Ended, fmt.Sprintf("%+v", got)
		})
	}
	c.RestartWorker(w)
	waitEvent(c, "the Worker to reconcile its new epoch", func(e EventRow) bool {
		return e.Type == domain.EventWorkerReconciled && e.WorkerID == w.ID && e.Sequence > mark
	})
	if !releaseFirst {
		if procident.Verify(in.PID, ident) != procident.Same {
			t.Fatalf("the harness of %v is not the one from before the crash", in.Execution)
		}
		if r := c.Run(c.Ctl, created.Run.ID); r.State != domain.RunRunning || r.Runtime != domain.RuntimeActive {
			t.Fatalf("the Run did not go on: %s", RunStage(r))
		}
		c.Ledger.ReleaseExecution(in.Execution)
	}

	if r := waitPublished(c, created.Run.ID); r.State != domain.RunCompleted {
		t.Errorf("Run = %s", RunStage(r))
	}
	if got := c.Ledger.Instances(token); len(got) != 1 || got[0].PID != in.PID {
		t.Errorf("the ledger has %+v, want only pid %d", got, in.PID)
	}

	Check(t, c, Expect{
		Tasks:       []ExpectedTask{{ID: created.Task.ID, Token: token}},
		DaemonKills: []DaemonKill{kill},
		Sampler:     sampler,
	})
}

// TestScenario_E2eV01_Particion (#11, as P-b): a partition of the Worker A
// that outlasts run_lost_timeout makes its Run lost and its retry run on B.
// When A comes back its process is an orphan: it is stopped, never run twice,
// the lost Run's result is published, and the retry completes.
// A is cut off both ways: Reject resets every connection, DropReplies lets
// its requests through but none of the answers, and resets them on healing.
func TestScenario_E2eV01_Particion(t *testing.T) {
	t.Run("reject", func(t *testing.T) { partition(t, netproxy.Reject) })
	t.Run("drop replies", func(t *testing.T) { partition(t, netproxy.DropReplies) })
}

func partition(t *testing.T, mode netproxy.Mode) {
	c := NewCluster(t, Opts{Workers: 2, MaxConcurrent: 1, Proxied: true})
	sampler := c.StartSampler()
	token := c.Ledger.Token()
	created := c.Do(c.Ledger.Script(token))
	c.TaskSet(created.Task.ID, "--retry-on-lost", "reschedule", "--retry-max-attempts", "2")
	run := created.Run.ID
	orphan := waitHarness(c, token)
	a := workerNamed(c, orphan.Worker)
	b := otherWorker(c, a)

	a.Proxy.SetMode(mode)
	lost := c.WaitRun(run, "lost", func(r v1.Run) bool { return r.State == domain.RunLost })
	var retry v1.Run
	c.Eventually("the retry to run on "+b.Name, func() (bool, string) {
		runs, err := c.API.Runs(c.H.Context(), client.RunQuery{Task: created.Task.ID})
		if err != nil {
			return false, err.Error()
		}
		i := slices.IndexFunc(runs, func(r v1.Run) bool { return r.Lineage == domain.LineageRetries && r.LineageParent == run })
		if i < 0 {
			return false, fmt.Sprintf("%d Runs, none retries %s", len(runs), run)
		}
		retry = runs[i]
		return retry.State == domain.RunRunning && retry.WorkerID == b.ID, RunStage(retry)
	})
	retried := Execution{Run: retry.ID, Epoch: 1, Worker: b.Name}
	c.Eventually("the harness of the retry to run", func() (bool, string) {
		got := c.Ledger.Instances(token)
		return slices.ContainsFunc(got, func(in Instance) bool { return in.Execution == retried && c.Ledger.Live(in) }), fmt.Sprintf("%+v", got)
	})
	if !c.Ledger.Live(orphan) {
		t.Fatalf("the harness of %v died in the partition", orphan.Execution)
	}
	if lost.Publication == domain.PublicationPublished {
		t.Fatalf("the lost Run was published while its Worker was cut off: %s", RunStage(lost))
	}

	// A comes back: its process is an orphan, stopped and never run again.
	a.Proxy.Heal()
	waitEvent(c, "the Run of "+a.Name+" to be orphaned", func(e EventRow) bool {
		return e.Type == domain.EventRunOrphaned && e.RunID == run
	})
	c.Eventually("the orphan on "+a.Name+" to be stopped", func() (bool, string) {
		return !c.Ledger.Live(orphan), fmt.Sprintf("pid %d alive", orphan.PID)
	})
	if r := waitPublished(c, run); r.State != domain.RunLost {
		t.Errorf("the lost Run came back as %s", r.State)
	}
	c.Ledger.ReleaseExecution(retried)
	if r := waitPublished(c, retry.ID); r.State != domain.RunCompleted {
		t.Errorf("the retry = %s", RunStage(r))
	}

	snap := c.ControllerSnapshot()
	for _, r := range []string{run, retry.ID} {
		if n := len(eventsOf(snap, r, domain.EventRunStarted)); n != 1 {
			t.Errorf("%s: %d RunStarted events", r, n)
		}
	}
	if n := len(eventsOf(snap, run, domain.EventRunOrphaned)); n != 1 {
		t.Errorf("%d RunOrphaned events", n)
	}
	got := c.Ledger.Instances(token)
	if len(got) != 2 || got[0].Execution != orphan.Execution || got[1].Execution != retried || got[0].Ended || !got[1].Ended {
		t.Errorf("the ledger has %+v, want the stopped %v and the ended %v", got, orphan.Execution, retried)
	}
	if n := c.Bypasses(); n != 0 {
		t.Errorf("%d connections went around the proxies", n)
	}

	Check(t, c, Expect{Tasks: []ExpectedTask{{ID: created.Task.ID, Token: token}}, Sampler: sampler})
}

// waitHarness waits for the one harness of token to be running, and returns it.
func waitHarness(c *Cluster, token string) Instance {
	c.t.Helper()
	var got Instance
	c.Eventually("the harness of "+token+" to run", func() (bool, string) {
		ins := c.Ledger.Instances(token)
		if len(ins) == 1 && c.Ledger.Live(ins[0]) {
			got = ins[0]
			return true, ""
		}
		return false, fmt.Sprintf("%+v", ins)
	})
	return got
}

// waitPublished waits for the Run to have settled and published its result.
func waitPublished(c *Cluster, run string) v1.Run {
	c.t.Helper()
	return c.WaitRun(run, "settled and published", func(r v1.Run) bool {
		return settled(r.State) && r.Publication == domain.PublicationPublished && r.Runtime != domain.RuntimeActive
	})
}

// waitEvent waits for an event of the Controller that matches, and returns the first.
func waitEvent(c *Cluster, what string, match func(EventRow) bool) EventRow {
	c.t.Helper()
	var got EventRow
	c.Eventually(what, func() (bool, string) {
		snap := c.ControllerSnapshot()
		i := slices.IndexFunc(snap.Events, match)
		if i < 0 {
			return false, fmt.Sprintf("%d events, none matches", len(snap.Events))
		}
		got = snap.Events[i]
		return true, ""
	})
	return got
}

// identities are the procident tokens of the processes of the instances.
func identities(t *testing.T, ins []Instance) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, in := range ins {
		ident, err := procident.StartOf(in.PID)
		if err != nil {
			t.Fatalf("identity of the harness of %v: %v", in.Execution, err)
		}
		out[in.PID] = ident
	}
	return out
}

func eventsOf(snap ControllerSnapshot, run string, typ domain.EventType) []EventRow {
	var out []EventRow
	for _, e := range snap.Events {
		if e.RunID == run && e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func lastEventSequence(snap ControllerSnapshot) int64 {
	if len(snap.Events) == 0 {
		return 0
	}
	return snap.Events[len(snap.Events)-1].Sequence
}

func expectedTasks(created []v1.CreateTaskResponse, tokens []string) []ExpectedTask {
	out := make([]ExpectedTask, len(created))
	for i, cr := range created {
		out[i] = ExpectedTask{ID: cr.Task.ID, Token: tokens[i]}
	}
	return out
}

func workerByID(c *Cluster, id string) *Worker {
	c.t.Helper()
	i := slices.IndexFunc(c.Workers, func(w *Worker) bool { return w.ID == id })
	if i < 0 {
		c.t.Fatalf("e2e: no Worker %q", id)
	}
	return c.Workers[i]
}

func workerNamed(c *Cluster, name string) *Worker {
	c.t.Helper()
	i := slices.IndexFunc(c.Workers, func(w *Worker) bool { return w.Name == name })
	if i < 0 {
		c.t.Fatalf("e2e: no Worker %q", name)
	}
	return c.Workers[i]
}

// otherWorker is a Worker of a two-Worker cluster that is not w.
func otherWorker(c *Cluster, w *Worker) *Worker {
	if c.Workers[0] == w {
		return c.Workers[1]
	}
	return c.Workers[0]
}

// runningIn are the names of the processes of box still running.
func runningIn(h *Harness, box *Box) []string {
	var out []string
	for _, p := range h.Procs() {
		if p.box == box && p.Running() {
			out = append(out, p.name)
		}
	}
	return out
}
