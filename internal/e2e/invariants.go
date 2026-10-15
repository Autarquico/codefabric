//go:build e2e && (linux || darwin)

package e2e

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// The invariants hold of a cluster once it is quiet: Check waits for that,
// reads what the cluster says of itself, and judges it against what the test
// knows it did. Every violation is gathered before the test fails, so that
// one failure tells everything that went wrong.

// quiesceLimit bounds the wait for a cluster to settle, before scaling.
const quiesceLimit = 120 * time.Second

// sampleEvery is the pause of a Sampler between two looks at the ledger.
const sampleEvery = 150 * time.Millisecond

// Expect is what the model of a test knows of what it did.
type Expect struct {
	// Tasks are every Task the test created.
	Tasks []ExpectedTask
	// DaemonKills are the kill -9 of Worker daemons, in order.
	DaemonKills []DaemonKill
	// Sampler, if the test ran one, is stopped by Check and its findings judged.
	Sampler *Sampler
	// Seed says how to repeat the test (its seed and the actions it chose), if it has one.
	Seed string
}

// ExpectedTask is a Task the test created.
type ExpectedTask struct {
	ID string
	// Token is the ledger token of its harnesses; "" if its prompt is no ledger script.
	Token string
	// Cancelled says that the test cancelled it.
	Cancelled bool
}

// DaemonKill is a kill -9 of the daemon of a Worker, as KillDaemon saw it once
// the daemon was dead: the journal's high-water mark and every daemon epoch
// that had written to it. A report of another epoch was written after the kill.
type DaemonKill struct {
	Worker    string
	HighWater int64
	Epochs    []string
}

// Observation is what a quiet cluster says of itself.
type Observation struct {
	Tasks     []v1.Task
	Runs      []v1.Run
	Snapshot  ControllerSnapshot
	WorkerIDs map[string]string // by Worker name
	// Instances are every harness the ledger saw, and Live those still running.
	Instances []Instance
	Live      []Instance
	// RelayRefs are the refs of the Project's relay repository, to their OIDs.
	RelayRefs map[string]string
	// Results are the result.txt of each published commit that has one.
	Results map[string]string
	// Sampled are the findings of the Sampler.
	Sampled []string
	// Unquiet is what still moved when the wait for quiet ran out; "" if it did not.
	Unquiet string
}

// Check waits for the cluster to be quiet and fails the test with every
// invariant it violates.
func Check(t *testing.T, c *Cluster, exp Expect) {
	t.Helper()
	obs := c.Observe(exp)
	for _, line := range diagnose(c) {
		t.Log(line)
	}
	found := Violations(obs, exp)
	if len(found) == 0 {
		return
	}
	seed := ""
	if exp.Seed != "" {
		seed = " (seed " + exp.Seed + ")"
	}
	t.Fatalf("e2e: %d invariant violations%s:\n  %s\n%s", len(found), seed, strings.Join(found, "\n  "), c.Logs())
}

// Observe waits, up to a bound, for the cluster to be quiet and reads it.
func (c *Cluster) Observe(exp Expect) Observation {
	c.t.Helper()
	began := time.Now()
	deadline := began.Add(Scaled(quiesceLimit))
	var obs Observation
	for {
		var moving string
		if obs, moving = c.look(); moving == "" {
			break
		}
		if time.Now().After(deadline) || c.H.Context().Err() != nil {
			obs.Unquiet = fmt.Sprintf("not quiet after %v: %s", time.Since(began).Round(time.Millisecond), moving)
			break
		}
		time.Sleep(4 * pollEvery)
	}
	if exp.Sampler != nil {
		obs.Sampled = exp.Sampler.Stop()
	}
	obs.WorkerIDs = map[string]string{}
	for _, w := range c.Workers {
		obs.WorkerIDs[w.Name] = w.ID
	}
	obs.RelayRefs, obs.Results = c.relay(obs.Runs)
	return obs
}

// look reads the cluster once and says what is still moving, "" if nothing.
func (c *Cluster) look() (obs Observation, moving string) {
	ctx := c.H.Context()
	var err error
	if obs.Tasks, err = c.API.Tasks(ctx, client.TaskQuery{Project: c.Project.ID, All: true}); err != nil {
		return obs, "listing Tasks: " + err.Error()
	}
	if obs.Runs, err = c.API.Runs(ctx, client.RunQuery{Project: c.Project.ID}); err != nil {
		return obs, "listing Runs: " + err.Error()
	}
	if obs.Snapshot, err = readController(ctx, c.Ctl.Paths.DatabaseFile()); err != nil {
		return obs, err.Error()
	}
	obs.Instances = c.Ledger.All()
	for _, in := range obs.Instances {
		if c.Ledger.Live(in) {
			obs.Live = append(obs.Live, in)
		}
	}
	return obs, stillMoving(obs)
}

// stillMoving says what keeps a cluster from being quiet: a Task or a Run not
// settled, a result being published, a process of a Run or a harness alive,
// a reservation held.
func stillMoving(obs Observation) string {
	for _, t := range obs.Tasks {
		if t.State == domain.TaskQueued || t.State == domain.TaskRunning {
			return fmt.Sprintf("Task %s is %s", t.ID, t.State)
		}
	}
	for _, r := range obs.Runs {
		if !settled(r.State) || r.Publication == domain.PublicationPending || r.Runtime == domain.RuntimeActive {
			return RunStage(r)
		}
	}
	if len(obs.Snapshot.OpenReservations) > 0 {
		return fmt.Sprintf("%d reservations open", len(obs.Snapshot.OpenReservations))
	}
	if len(obs.Live) > 0 {
		return fmt.Sprintf("the harness of %v alive", obs.Live[0].Execution)
	}
	return ""
}

// relay reads the refs of the relay and the result.txt of each published commit.
func (c *Cluster) relay(runs []v1.Run) (refs, results map[string]string) {
	c.t.Helper()
	refs, results = map[string]string{}, map[string]string{}
	out := gittest.Run(c.t, c.RelayRepo(), "for-each-ref", "--format=%(objectname) %(refname)")
	for line := range strings.Lines(out) {
		if oid, ref, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			refs[ref] = oid
		}
	}
	for _, r := range runs {
		if r.ResultCommit == "" {
			continue
		}
		if text, err := gittest.Try(c.t, c.RelayRepo(), "show", r.ResultCommit+":result.txt"); err == nil {
			results[r.ResultCommit] = text
		}
	}
	return refs, results
}

// diagnose are the lines of the daemons' logs that say a worker_seq was used
// again: a lead, not a verdict (the journal invariants are the verdict).
func diagnose(c *Cluster) []string {
	procs := []*Proc{c.Controller}
	for _, w := range c.Workers {
		procs = append(procs, w.Daemon)
	}
	var out []string
	for _, p := range procs {
		for line := range strings.Lines(p.Log()) {
			if strings.Contains(line, "worker_seq reused") {
				out = append(out, "e2e: diagnostic: "+p.LogPath()+": "+strings.TrimSpace(line))
			}
		}
	}
	return out
}

// KillDaemon is kill -9 of the daemon of w, as a crash, and what its journal
// held once it was dead. Restart it with RestartWorker.
func (c *Cluster) KillDaemon(w *Worker) DaemonKill {
	c.t.Helper()
	w.Daemon.Kill9()
	ws := c.WorkerSnapshot(w)
	epochs := map[string]bool{}
	for _, e := range ws.JournalEpochs {
		epochs[e] = true
	}
	for _, e := range journalOf(c.ControllerSnapshot(), w.ID) {
		epochs[e.epoch] = true
	}
	return DaemonKill{Worker: w.Name, HighWater: ws.JournalSeq, Epochs: slices.Sorted(maps.Keys(epochs))}
}

// Sampler looks at the ledger now and then while a test runs: a Run with two
// harnesses alive at once is evidence, though only the ledger is proof.
type Sampler struct {
	stop, done chan struct{}
	once       sync.Once

	mu       sync.Mutex
	found    []string
	reported map[string]bool
}

// StartSampler starts a Sampler; Check stops it, and so does the end of the test.
func (c *Cluster) StartSampler() *Sampler {
	s := &Sampler{stop: make(chan struct{}), done: make(chan struct{}), reported: map[string]bool{}}
	go func() {
		defer close(s.done) // the ledger fails the test with Fatal, which ends this goroutine
		tick := time.NewTicker(sampleEvery)
		defer tick.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-tick.C:
				s.sample(c.Ledger)
			}
		}
	}()
	c.t.Cleanup(func() { s.Stop() })
	return s
}

func (s *Sampler) sample(l *Ledger) {
	live := map[string][]string{}
	for _, in := range l.All() {
		if l.Live(in) {
			live[in.Run] = append(live[in.Run], fmt.Sprintf("%v pid %d", in.Execution, in.PID))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for run, ins := range live {
		if len(ins) > 1 && !s.reported[run] {
			s.reported[run] = true
			s.found = append(s.found, fmt.Sprintf("[3 one harness] at %s Run %s had %d harnesses alive: %s",
				time.Now().Format("15:04:05.000"), run, len(ins), strings.Join(ins, "; ")))
		}
	}
}

// Stop stops the Sampler and returns what it found.
func (s *Sampler) Stop() []string {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.found)
}

// Violations are the invariants obs breaks, given what the test did; none if it is sound.
func Violations(obs Observation, exp Expect) []string {
	v := &violations{}
	if obs.Unquiet != "" {
		v.add("[quiet] %s", obs.Unquiet)
	}
	m := newModel(obs, exp)
	checkTasks(m, v)
	checkRuns(m, v)
	checkRetries(m, v)
	checkHarnesses(m, v)
	checkPublication(m, v)
	checkJournal(m, v)
	checkReservations(m, v)
	slices.Sort(v.found)
	return v.found
}

type violations struct{ found []string }

func (v *violations) add(format string, args ...any) {
	v.found = append(v.found, fmt.Sprintf(format, args...))
}

// model is an observation indexed for the checks.
type model struct {
	Observation
	exp      Expect
	expected map[string]ExpectedTask // by Task ID
	tasks    map[string]v1.Task      // by ID
	runs     map[string]v1.Run       // by ID
	byTask   map[string][]v1.Run     // by Task ID, by attempt
	events   map[string][]EventRow   // by Run ID
	harness  map[string][]Instance   // by Run ID
}

func newModel(obs Observation, exp Expect) model {
	m := model{
		Observation: obs, exp: exp,
		expected: map[string]ExpectedTask{}, tasks: map[string]v1.Task{}, runs: map[string]v1.Run{},
		byTask: map[string][]v1.Run{}, events: map[string][]EventRow{}, harness: map[string][]Instance{},
	}
	for _, t := range exp.Tasks {
		m.expected[t.ID] = t
	}
	for _, t := range obs.Tasks {
		m.tasks[t.ID] = t
	}
	for _, r := range obs.Runs {
		m.runs[r.ID] = r
		m.byTask[r.TaskID] = append(m.byTask[r.TaskID], r)
	}
	for _, runs := range m.byTask {
		slices.SortFunc(runs, func(a, b v1.Run) int { return cmp.Compare(a.Attempt, b.Attempt) })
	}
	for _, e := range obs.Snapshot.Events {
		if e.RunID != "" {
			m.events[e.RunID] = append(m.events[e.RunID], e)
		}
	}
	for _, in := range obs.Instances {
		if in.Worker != "" {
			m.harness[in.Run] = append(m.harness[in.Run], in)
		}
	}
	return m
}

// checkTasks: every Task is one the test created, and each settled the way
// its Runs and what the test did allow (§11.7).
func checkTasks(m model, v *violations) {
	for _, t := range m.Tasks {
		e, ok := m.expected[t.ID]
		switch {
		case !ok:
			v.add("[1 tasks] Task %s exists, but the test created no such Task", t.ID)
		case t.State == domain.TaskCancelled && !e.Cancelled:
			v.add("[1 tasks] Task %s is cancelled, but the test did not cancel it", t.ID)
		case e.Cancelled && t.State != domain.TaskCancelled:
			v.add("[1 tasks] Task %s was cancelled by the test, but is %s", t.ID, taskState(t))
		case t.State == domain.TaskFailed:
			runs := m.byTask[t.ID]
			if len(runs) == 0 || (runs[len(runs)-1].State != domain.RunFailed && runs[len(runs)-1].State != domain.RunLost) {
				v.add("[1 tasks] Task %s is failed, but its last Run neither failed nor was lost", t.ID)
			}
		case t.State == domain.TaskCompleted, t.State == domain.TaskCancelled,
			t.State == domain.TaskWaiting && t.WaitingReason == domain.WaitingDecision:
		default:
			v.add("[1 tasks] Task %s ended %s", t.ID, taskState(t))
		}
	}
	for _, e := range m.exp.Tasks {
		if _, ok := m.tasks[e.ID]; !ok {
			v.add("[1 tasks] Task %s, which the test created, is missing", e.ID)
		}
	}
}

func taskState(t v1.Task) string {
	if t.WaitingReason != domain.WaitingNone {
		return fmt.Sprintf("%s(%s)", t.State, t.WaitingReason)
	}
	return string(t.State)
}

// terminalEvent is the one event that ends a Run in each settled state;
// RunExited{after_end}, RunStopped, RunSealed and RunPublished end none.
var terminalEvent = map[domain.RunState]domain.EventType{
	domain.RunCompleted: domain.EventRunCompleted,
	domain.RunFailed:    domain.EventRunFailed,
	domain.RunCancelled: domain.EventRunCancelled,
	domain.RunLost:      domain.EventRunLost,
}

func endsRun(t domain.EventType) bool {
	for _, end := range terminalEvent {
		if t == end {
			return true
		}
	}
	return false
}

func settled(s domain.RunState) bool {
	_, ok := terminalEvent[s]
	return ok
}

// checkRuns: every Run settled with the cause its state demands, started at
// most once, ended exactly once, and no Task has two Runs completed.
func checkRuns(m model, v *violations) {
	accepted := map[string]int{}
	for _, l := range m.Snapshot.Leases {
		if l.State == string(domain.LeaseAccepted) {
			accepted[l.RunID]++
		}
	}
	for _, r := range m.Runs {
		if _, ok := m.tasks[r.TaskID]; !ok {
			v.add("[1 runs] Run %s is of Task %s, which is not listed", r.ID, r.TaskID)
		}
		checkCause(m, v, r)
		checkEvents(v, r, m.events[r.ID])
		if n := accepted[r.ID]; n > 1 {
			v.add("[2 one lease] Run %s has %d accepted leases", r.ID, n)
		}
	}
	for task, runs := range m.byTask {
		var completed []string
		for _, r := range runs {
			if r.State == domain.RunCompleted {
				completed = append(completed, r.ID)
			}
		}
		if n := len(completed); n > 1 {
			v.add("[2 one completed] Task %s has %d completed Runs %v", task, n, completed)
		}
	}
}

// checkCause: the cause of an end depends on the state (lost and cancelled
// leave end_reason empty); a cancelled Run is one the test asked for.
func checkCause(m model, v *violations, r v1.Run) {
	if settled(r.State) && r.FinishedAt == nil {
		v.add("[1 runs] Run %s is %s without finished_at", r.ID, r.State)
	}
	switch r.State {
	case domain.RunCompleted:
		if r.EndReason != domain.EndExitCode || r.ExitCode == nil || *r.ExitCode != 0 {
			v.add("[1 runs] Run %s is completed with end_reason %q and exit code %s", r.ID, r.EndReason, exitCode(r.ExitCode))
		}
	case domain.RunFailed:
		if r.EndReason == "" {
			v.add("[1 runs] Run %s failed without an end_reason", r.ID)
		}
	case domain.RunCancelled:
		if !m.expected[r.TaskID].Cancelled {
			v.add("[1 runs] Run %s is cancelled, but the test did not cancel its Task %s", r.ID, r.TaskID)
		}
	case domain.RunLost:
	default:
		v.add("[1 runs] Run %s is still %s (%s)", r.ID, r.State, RunStage(r))
	}
}

func exitCode(code *int) string {
	if code == nil {
		return "none"
	}
	return fmt.Sprint(*code)
}

// checkEvents: at most one RunStarted (none for a Run cancelled while queued
// or failed before its session), exactly one end, and the events that happen
// once to a Run, once.
func checkEvents(v *violations, r v1.Run, events []EventRow) {
	count := map[domain.EventType]int{}
	orphaned := map[uint64]int{}
	var ends []string
	for _, e := range events {
		count[e.Type]++
		if e.Type == domain.EventRunOrphaned {
			orphaned[e.LeaseEpoch]++
		}
		if endsRun(e.Type) {
			ends = append(ends, string(e.Type))
		}
	}
	if n := count[domain.EventRunStarted]; n > 1 {
		v.add("[2 one start] Run %s has %d RunStarted", r.ID, n)
	}
	want, ok := terminalEvent[r.State]
	switch {
	case len(ends) != 1 && (ok || len(ends) > 1):
		v.add("[2 one end] Run %s (%s) has %d terminal events %v", r.ID, r.State, len(ends), ends)
	case len(ends) == 1 && ends[0] != string(want):
		v.add("[2 one end] Run %s is %s, but it ended with %s", r.ID, r.State, ends[0])
	}
	for _, t := range []domain.EventType{domain.EventRunSealed, domain.EventRunPublished, domain.EventRunLost, domain.EventRunStopped} {
		if n := count[t]; n > 1 {
			v.add("[8 once] Run %s has %d %s", r.ID, n, t)
		}
	}
	for epoch, n := range orphaned {
		if n > 1 {
			v.add("[8 once] Run %s has %d RunOrphaned under lease epoch %d", r.ID, n, epoch)
		}
	}
}

// retryAction is what the retry policy does after a Run that ended in state.
func retryAction(p v1.Retry, state domain.RunState) domain.RetryAction {
	switch state {
	case domain.RunLost:
		return p.OnLost
	case domain.RunFailed:
		return p.OnFailed
	}
	return domain.RetryNone
}

// checkRetries: a retry exists exactly when the policy, the attempts left and
// the Task ask for it (§11.7 row 12: not cancelled, nothing completed, the
// last Run failed or lost under reschedule), and retries a Run once.
func checkRetries(m model, v *violations) {
	for taskID, runs := range m.byTask {
		t, ok := m.tasks[taskID]
		if !ok {
			continue
		}
		retries := map[string]int{}
		for _, r := range runs {
			if r.Lineage != domain.LineageRetries {
				continue
			}
			retries[r.LineageParent]++
			parent, ok := m.runs[r.LineageParent]
			if !ok || parent.TaskID != taskID || retryAction(t.Retry, parent.State) != domain.RetryReschedule {
				v.add("[1 retry] Run %s retries %s, which the policy of Task %s does not retry", r.ID, r.LineageParent, taskID)
			}
		}
		for parent, n := range retries {
			if n > 1 {
				v.add("[2 one retry] Run %s was retried %d times", parent, n)
			}
		}
		last := runs[len(runs)-1]
		done := slices.ContainsFunc(runs, func(r v1.Run) bool { return r.State == domain.RunCompleted })
		if !done && t.CancelledAt == nil && t.ArchivedAt == nil && t.CompletedManuallyAt == nil &&
			retryAction(t.Retry, last.State) == domain.RetryReschedule && t.Retry.MaxAttempts > len(runs) {
			v.add("[1 retry] Run %s of Task %s is %s with %d attempts left, but has no retry",
				last.ID, taskID, last.State, t.Retry.MaxAttempts-len(runs))
		}
	}
}

// checkHarnesses: each harness is of a known execution (Run, lease epoch,
// Worker) under an accepted lease of that Worker and is the Task's; a Run
// had at most one, and none lives on.
func checkHarnesses(m model, v *violations) {
	tokenTask := map[string]string{}
	for _, t := range m.exp.Tasks {
		if t.Token != "" {
			tokenTask[t.Token] = t.ID
		}
	}
	for _, in := range m.Instances {
		if in.Worker == "" {
			v.add("[3 one harness] a harness of %s (pid %d) ran in %s, the worktree of no Worker", in.Token, in.PID, in.Dir)
			continue
		}
		if task, ok := tokenTask[in.Token]; !ok || m.runs[in.Run].TaskID != task {
			v.add("[3 one harness] the harness of %s ran %v, a Run of another Task", in.Token, in.Execution)
		}
		wid := m.WorkerIDs[in.Worker]
		if !slices.ContainsFunc(m.Snapshot.Leases, func(l LeaseRow) bool {
			return l.RunID == in.Run && l.Epoch == in.Epoch && l.WorkerID == wid && l.State == string(domain.LeaseAccepted)
		}) {
			v.add("[3 one harness] the harness %v ran without an accepted lease of that Worker", in.Execution)
		}
	}
	for run, ins := range m.harness {
		if len(ins) > 1 {
			var who []string
			for _, in := range ins {
				who = append(who, fmt.Sprintf("%v pid %d", in.Execution, in.PID))
			}
			v.add("[3 one harness] Run %s had %d harnesses: %s", run, len(ins), strings.Join(who, "; "))
		}
	}
	for _, in := range m.Live {
		v.add("[7 none left] the harness %v (pid %d) is alive once the cluster is quiet", in.Execution, in.PID)
	}
	v.found = append(v.found, m.Sampled...)
}

// checkPublication: a Run whose harness ran has its result published (a lost
// one too, in Phase 1), at the tip of its own branch, with the content its
// harness wrote if it finished; a preserved orphan has its ref and no other.
func checkPublication(m model, v *violations) {
	for _, r := range m.Runs {
		ins := m.harness[r.ID]
		if len(ins) > 0 && r.Publication != domain.PublicationPublished {
			v.add("[4 publication] Run %s ran its harness, but its publication is %s", r.ID, r.Publication)
		}
		if r.Publication != domain.PublicationPublished {
			continue
		}
		if r.ResultCommit == "" || r.Branch == "" {
			v.add("[4 publication] Run %s is published without a result_commit or branch", r.ID)
			continue
		}
		if tip := m.RelayRefs["refs/heads/"+r.Branch]; tip != r.ResultCommit {
			v.add("[4 publication] Run %s published %s, but refs/heads/%s is %q", r.ID, r.ResultCommit, r.Branch, tip)
		}
		for _, in := range ins {
			if !in.Ended {
				continue
			}
			got, ok := m.Results[r.ResultCommit]
			if got = strings.TrimSpace(got); !ok || got != in.Token {
				v.add("[4 publication] Run %s published %s with result.txt %q, want %q", r.ID, r.ResultCommit, got, in.Token)
			}
		}
	}
	preserved := map[string]string{}
	for _, o := range m.Snapshot.Orphans {
		if o.Preserved == "relay" {
			preserved[domain.OrphanRef(domain.ID(o.RunID), o.LeaseEpoch)] = o.ResultCommit
		}
	}
	for ref, commit := range preserved {
		if got := m.RelayRefs[ref]; got != commit {
			v.add("[4 publication] the orphan preserved at %s is %s, but the relay has %q", ref, commit, got)
		}
	}
	for ref := range m.RelayRefs {
		if _, ok := preserved[ref]; domain.ValidOrphanRef(ref) && !ok {
			v.add("[4 publication] the relay has %s, an orphan the Controller did not preserve", ref)
		}
	}
}

// triple is the identity of a report entry: the Worker, its daemon epoch and
// the seq of the entry in its journal.
type triple struct {
	worker, epoch string
	seq           int64
}

// journalOf are the distinct triples of worker in the events and receipts.
func journalOf(snap ControllerSnapshot, worker string) []triple {
	seen := map[triple]bool{}
	for _, e := range snap.Events {
		if e.WorkerSeq != nil && e.WorkerID == worker {
			seen[triple{e.WorkerID, e.WorkerEpoch, *e.WorkerSeq}] = true
		}
	}
	for _, r := range snap.Receipts {
		if r.WorkerID == worker {
			seen[triple{r.WorkerID, r.WorkerEpoch, r.WorkerSeq}] = true
		}
	}
	return slices.Collect(maps.Keys(seen))
}

// checkJournal: a report entry is one logical record, at most one row in each
// table and both of the same Run and digest (5a); a seq belongs to one epoch
// of its Worker (5b); after a daemon died, its next epoch reported above the
// high-water mark of the journal and never at or under it (5c).
func checkJournal(m model, v *violations) {
	events := map[triple][]EventRow{}
	for _, e := range m.Snapshot.Events {
		if e.WorkerSeq != nil { // events with no Worker triple are no report's
			k := triple{e.WorkerID, e.WorkerEpoch, *e.WorkerSeq}
			events[k] = append(events[k], e)
		}
	}
	receipts := map[triple][]ReceiptRow{}
	for _, r := range m.Snapshot.Receipts {
		k := triple{r.WorkerID, r.WorkerEpoch, r.WorkerSeq}
		receipts[k] = append(receipts[k], r)
	}
	epochsOf := map[string]map[int64]map[string]bool{} // worker, seq, epochs
	note := func(k triple) {
		if epochsOf[k.worker] == nil {
			epochsOf[k.worker] = map[int64]map[string]bool{}
		}
		if epochsOf[k.worker][k.seq] == nil {
			epochsOf[k.worker][k.seq] = map[string]bool{}
		}
		epochsOf[k.worker][k.seq][k.epoch] = true
	}
	for k, es := range events {
		note(k)
		if len(es) > 1 {
			v.add("[5a journal] %v has %d events", k, len(es))
		}
		if rs := receipts[k]; len(rs) > 0 && (rs[0].RunID != es[0].RunID || rs[0].Digest != es[0].ReportDigest) {
			v.add("[5a journal] %v: the event says Run %s digest %q, the receipt Run %s digest %q",
				k, es[0].RunID, es[0].ReportDigest, rs[0].RunID, rs[0].Digest)
		}
	}
	for k, rs := range receipts {
		note(k)
		if len(rs) > 1 {
			v.add("[5a journal] %v has %d receipts", k, len(rs))
		}
	}
	for worker, seqs := range epochsOf {
		for seq, epochs := range seqs {
			if len(epochs) > 1 {
				v.add("[5b journal] seq %d of Worker %s is in epochs %v", seq, worker, slices.Sorted(maps.Keys(epochs)))
			}
		}
	}
	for _, kill := range m.exp.DaemonKills {
		checkRestart(m, v, kill)
	}
}

func checkRestart(m model, v *violations, kill DaemonKill) {
	wid, ok := m.WorkerIDs[kill.Worker]
	if !ok {
		v.add("[5c journal] the killed daemon of %s is of no Worker", kill.Worker)
		return
	}
	fresh := 0
	for _, k := range journalOf(m.Snapshot, wid) {
		if slices.Contains(kill.Epochs, k.epoch) {
			continue // written before the kill, replayed or not
		}
		if k.seq <= kill.HighWater {
			v.add("[5c journal] %s reported seq %d in the new epoch %s, not above the high-water mark %d of its kill",
				kill.Worker, k.seq, k.epoch, kill.HighWater)
		} else {
			fresh++
		}
	}
	if fresh == 0 {
		v.add("[5c journal] %s reported nothing in a new epoch after its kill at high-water mark %d", kill.Worker, kill.HighWater)
	}
}

// checkReservations: no reservation is held once the cluster is quiet; a lost
// Run holds its own only until its Worker returns, stops and seals it.
func checkReservations(m model, v *violations) {
	for _, r := range m.Snapshot.OpenReservations {
		v.add("[6 reservations] the reservation of Run %s on Worker %s (lease %s) is open", r.RunID, r.WorkerID, r.LeaseID)
	}
}
