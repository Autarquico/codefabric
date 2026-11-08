//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/e2e/netproxy"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// recoveryRuns is how many Runs are in flight: two per Worker.
const recoveryRuns = 4

// recoveryCut is how long a cut lasts at least. Every network cut here ends
// well under run_lost_timeout (8 s before CF_E2E_SCALE), so that no Run is
// lost and what comes back is replay and reconciliation.
const recoveryCut = 2 * time.Second

// recoveryStep is the pause between the parts of a cut that overlap.
const recoveryStep = time.Second

// recoveryGrowth is how many lines a Run must print after a cut before its
// history is read again: the output has to flow again, not only exist.
const recoveryGrowth = 2

// history is what an observer can read of a Run: its events, its structured
// logs and its output, each in the order the Controller shows them.
type history struct {
	events []string
	logs   []string
	lines  []string // the complete "rec <token> line N" lines of the output
	// problems are what is wrong with it by itself: a gap, a repetition, an
	// order that is not the Worker's.
	problems []string
}

func (h history) String() string {
	return fmt.Sprintf("%d events, %d logs, %d lines", len(h.events), len(h.logs), len(h.lines))
}

// TestScenario_E2eV03_DoneDeFase4 (Done de Fase 4): four Runs in flight, and
// the Controller killed with -9 while a Worker is cut off and the daemon of the
// other one is killed too; then a daemon killed while the other Worker is cut
// off; then the Controller killed again while two harnesses end. No Run is
// lost or run twice, each ends as it should, and what an observer read before
// each cut (events, logs, output) is a prefix of what it reads afterwards: no
// gap, no repetition, same order.
func TestScenario_E2eV03_DoneDeFase4(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 2, MaxConcurrent: 2, Proxied: true})
	sampler := c.StartSampler()
	w1, w2 := c.Workers[0], c.Workers[1]

	tokens := make([]string, recoveryRuns)
	runs := make([]string, recoveryRuns)
	var created []v1.CreateTaskResponse
	for i := range tokens {
		tokens[i] = c.Ledger.Token()
		created = append(created, c.Do(recoveryScript(c, tokens[i])))
		runs[i] = created[i].Run.ID
	}
	idents := map[string]string{}
	pids := map[string]int{}
	for _, token := range tokens {
		in := waitHarness(c, token)
		pids[token] = in.PID
		idents[token] = identities(t, []Instance{in})[in.PID]
	}
	for i := range runs {
		recoveryWaitLines(c, runs[i], tokens[i], 3)
	}
	before := recoveryRead(c, runs, tokens)
	recoveryRequireSane(t, "before any cut", before)

	// Cut 1: the Controller dies, the network of w1 goes with it, and the
	// daemon of w2 dies while the Controller is down.
	mark := lastEventSequence(c.ControllerSnapshot())
	c.Controller.Kill9()
	cut := time.Now()
	w1.Proxy.SetMode(netproxy.Reject)
	kills := []DaemonKill{c.KillDaemon(w2)}
	time.Sleep(Scaled(recoveryStep))
	c.RestartController()
	c.RestartWorker(w2)
	time.Sleep(max(0, Scaled(recoveryCut)-time.Since(cut)))
	w1.Proxy.Heal()
	recoveryAwaitReconciled(c, mark, w1, w2)
	recoveryRequireRunning(c, runs, tokens, pids, idents)
	before = recoveryAfterCut(t, c, "cut 1 (Controller, network of w1, daemon of w2)", before, runs, tokens)

	// Cut 2: the daemon of w1 dies and comes back while w2 is cut off.
	mark = lastEventSequence(c.ControllerSnapshot())
	cut = time.Now()
	w2.Proxy.SetMode(netproxy.Reject)
	kills = append(kills, c.KillDaemon(w1))
	time.Sleep(Scaled(recoveryStep))
	c.RestartWorker(w1)
	time.Sleep(max(0, Scaled(recoveryCut)-time.Since(cut)))
	w2.Proxy.Heal()
	recoveryAwaitReconciled(c, mark, w1)
	recoveryRequireRunning(c, runs, tokens, pids, idents)
	before = recoveryAfterCut(t, c, "cut 2 (daemon of w1, network of w2)", before, runs, tokens)

	// Cut 3: the Controller dies once more, and two harnesses end while it is down.
	c.Controller.Kill9()
	c.Ledger.Release(tokens[0])
	c.Ledger.Release(tokens[1])
	c.Eventually("the two released harnesses to end while the Controller is down", func() (bool, string) {
		a, b := c.Ledger.Instances(tokens[0]), c.Ledger.Instances(tokens[1])
		return len(a) == 1 && a[0].Ended && len(b) == 1 && b[0].Ended, fmt.Sprintf("%+v %+v", a, b)
	})
	time.Sleep(Scaled(recoveryStep))
	c.RestartController()
	for _, run := range runs[:2] {
		if r := waitPublished(c, run); r.State != domain.RunCompleted {
			t.Errorf("Run %s = %s", run, RunStage(r))
		}
	}
	for _, token := range tokens[2:] {
		c.Ledger.Release(token)
	}
	for _, run := range runs[2:] {
		if r := waitPublished(c, run); r.State != domain.RunCompleted {
			t.Errorf("Run %s = %s", run, RunStage(r))
		}
	}

	// The end: the full history of each Run extends what was read before.
	var after map[string]history
	c.Eventually("the whole output of every Run to arrive", func() (bool, string) {
		after = recoveryRead(c, runs, tokens)
		for i, run := range runs {
			out := c.mustCF(c.Observer, "logs", "--run", run, "--tail", "1000").Stdout
			if !strings.Contains(out, "e2e: finished "+tokens[i]) {
				return false, "the end of the output of " + run + " is missing"
			}
			if len(after[run].logs) == 0 {
				return false, "no structured log of " + run + " yet"
			}
		}
		return true, ""
	})
	recoveryRequireSane(t, "at the end", after)
	recoveryRequirePrefix(t, "at the end", before, after)

	snap := c.ControllerSnapshot()
	for _, run := range runs {
		for _, typ := range []domain.EventType{domain.EventRunStarted, domain.EventRunCompleted, domain.EventRunSealed, domain.EventRunPublished} {
			if n := len(eventsOf(snap, run, typ)); n != 1 {
				t.Errorf("%s: %d %s events, want 1", run, n, typ)
			}
		}
		if n := countFunc(snap.Leases, func(l LeaseRow) bool { return l.RunID == run }); n != 1 {
			t.Errorf("%s: %d leases, want 1", run, n)
		}
	}
	for i, token := range tokens {
		if got := c.Ledger.Instances(token); len(got) != 1 || got[0].PID != pids[token] || !got[0].Ended {
			t.Errorf("the ledger of %s has %+v, want only the ended pid %d (Run %s)", token, got, pids[token], runs[i])
		}
	}
	if n := c.Bypasses(); n != 0 {
		t.Errorf("%d connections went around the proxies", n)
	}
	Check(t, c, Expect{Tasks: expectedTasks(created, tokens), DaemonKills: kills, Sampler: sampler})
}

// recoveryScript is the prompt of a Task: the ledger's script, with a side
// that prints a numbered line every 200 ms until it is released. The harness
// waits for that side to stop before it ends, so no line is printed after it.
func recoveryScript(c *Cluster, token string) string {
	return fmt.Sprintf(`trap wait EXIT
( i=0; while [ ! -e '%s/go/%s' ] && [ "$i" -lt 600 ]; do i=$((i+1)); echo "rec %s line $i"; sleep 0.2; done ) &
`, c.Ledger.dir, token, token) + c.Ledger.Script(token)
}

// recoveryWaitLines waits for the Controller to have n lines of the Run's output.
func recoveryWaitLines(c *Cluster, run, token string, n int) {
	c.t.Helper()
	c.Eventually(fmt.Sprintf("%d lines of the output of %s", n, run), func() (bool, string) {
		got := recoveryLines(c, run, token)
		return len(got) >= n, fmt.Sprint(len(got))
	})
}

// recoveryLines are the complete numbered lines of the Run's output, as the
// Controller shows them.
func recoveryLines(c *Cluster, run, token string) []string {
	c.t.Helper()
	text := c.mustCF(c.Observer, "logs", "--run", run, "--tail", "1000").Stdout
	parts := strings.Split(text, "\n")
	parts = parts[:len(parts)-1] // what follows the last newline is not a complete line
	var out []string
	for _, l := range parts {
		if l = strings.TrimRight(l, "\r"); strings.HasPrefix(l, "rec "+token+" line ") {
			out = append(out, l)
		}
	}
	return out
}

// recoveryRead is what an observer reads of each Run now, by Run.
func recoveryRead(c *Cluster, runs, tokens []string) map[string]history {
	c.t.Helper()
	snap := c.ControllerSnapshot()
	out := map[string]history{}
	for i, run := range runs {
		var h history
		for _, e := range snap.Events {
			if e.RunID == run {
				seq := "-"
				if e.WorkerSeq != nil {
					seq = strconv.FormatInt(*e.WorkerSeq, 10)
				}
				h.events = append(h.events, fmt.Sprintf("#%d %s %s/%s/%s", e.Sequence, e.Type, e.WorkerID, e.WorkerEpoch, seq))
			}
		}
		for _, l := range runLogs(c, run) {
			seq := "-"
			if l.WorkerSeq != nil {
				seq = strconv.FormatUint(*l.WorkerSeq, 10)
			}
			h.logs = append(h.logs, fmt.Sprintf("#%d %s/%s/%s %s", l.Sequence, l.WorkerID, l.WorkerEpoch, seq, l.Message))
		}
		h.lines = recoveryLines(c, run, tokens[i])
		h.problems = recoveryProblems(snap, c, run, h)
		out[run] = h
	}
	return out
}

// recoveryProblems says what is wrong with a history by itself: output lines
// that are not 1, 2, 3, ... (a gap, a repetition, another order), an event or
// log twice, a worker_seq that does not grow within its epoch.
func recoveryProblems(snap ControllerSnapshot, c *Cluster, run string, h history) []string {
	var found []string
	for i, l := range h.lines {
		if want := fmt.Sprintf(" line %d", i+1); !strings.HasSuffix(l, want) {
			found = append(found, fmt.Sprintf("output line %d is %q: a gap, a repetition or a swap", i+1, l))
			break
		}
	}
	seen := map[string]bool{}
	last := map[string]int64{}
	prev := int64(0)
	for _, e := range snap.Events {
		if e.RunID != run {
			continue
		}
		if e.Sequence <= prev {
			found = append(found, fmt.Sprintf("event #%d after #%d", e.Sequence, prev))
		}
		prev = e.Sequence
		if e.WorkerSeq == nil {
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", e.WorkerID, e.WorkerEpoch, *e.WorkerSeq)
		if seen[key] {
			found = append(found, "event "+key+" twice")
		}
		seen[key] = true
		epoch := e.WorkerID + "/" + e.WorkerEpoch
		if *e.WorkerSeq <= last[epoch] {
			found = append(found, fmt.Sprintf("event %s: worker_seq not after %d", key, last[epoch]))
		}
		last[epoch] = *e.WorkerSeq
	}
	seenLogs := map[string]bool{}
	for _, l := range runLogs(c, run) {
		if l.WorkerSeq == nil {
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", l.WorkerID, l.WorkerEpoch, *l.WorkerSeq)
		if seenLogs[key] {
			found = append(found, "log "+key+" twice")
		}
		seenLogs[key] = true
	}
	return found
}

func recoveryRequireSane(t *testing.T, when string, now map[string]history) {
	t.Helper()
	for run, h := range now {
		for _, p := range h.problems {
			t.Errorf("%s: Run %s: %s", when, run, p)
		}
	}
}

// recoveryRequirePrefix fails for each Run whose earlier history is not the
// beginning of its history now, for events, logs and output alike.
func recoveryRequirePrefix(t *testing.T, when string, before, after map[string]history) {
	t.Helper()
	for run, b := range before {
		a := after[run]
		for _, part := range []struct {
			name     string
			was, now []string
		}{{"events", b.events, a.events}, {"logs", b.logs, a.logs}, {"output", b.lines, a.lines}} {
			if len(part.now) < len(part.was) || !slices.Equal(part.was, part.now[:len(part.was)]) {
				t.Errorf("%s: Run %s: the %s read before are not a prefix of those now (%d before, %d now); first difference: %s",
					when, run, part.name, len(part.was), len(part.now), firstDifference(part.was, part.now))
			}
		}
	}
}

func firstDifference(was, now []string) string {
	for i, w := range was {
		if i >= len(now) {
			return fmt.Sprintf("%q is missing now", w)
		}
		if w != now[i] {
			return fmt.Sprintf("index %d was %q, is %q", i, w, now[i])
		}
	}
	return "none"
}

// recoveryAfterCut waits for the output of every Run to flow again, reads the
// histories, requires them sane and extending those of before, and returns them.
func recoveryAfterCut(t *testing.T, c *Cluster, when string, before map[string]history, runs, tokens []string) map[string]history {
	t.Helper()
	for i, run := range runs {
		recoveryWaitLines(c, run, tokens[i], len(before[run].lines)+recoveryGrowth)
	}
	after := recoveryRead(c, runs, tokens)
	recoveryRequireSane(t, when, after)
	recoveryRequirePrefix(t, when, before, after)
	for i, run := range runs {
		t.Logf("%s: %s (%s): %s", when, run, tokens[i], after[run])
	}
	return after
}

// recoveryAwaitReconciled waits for each Worker to reconcile after mark.
func recoveryAwaitReconciled(c *Cluster, mark int64, workers ...*Worker) {
	c.t.Helper()
	for _, w := range workers {
		waitEvent(c, "Worker "+w.Name+" to reconcile", func(e EventRow) bool {
			return e.Type == domain.EventWorkerReconciled && e.WorkerID == w.ID && e.Sequence > mark
		})
	}
}

// recoveryRequireRunning fails for a Run that is not running, or whose harness
// is not the very process it had before the cut.
func recoveryRequireRunning(c *Cluster, runs, tokens []string, pids map[string]int, idents map[string]string) {
	c.t.Helper()
	for i, run := range runs {
		if r := c.Run(c.Ctl, run); r.State != domain.RunRunning || r.Runtime != domain.RuntimeActive {
			c.t.Fatalf("Run %s did not go on: %s", run, RunStage(r))
		}
		if got := c.Ledger.Instances(tokens[i]); len(got) != 1 || got[0].PID != pids[tokens[i]] || procident.Verify(pids[tokens[i]], idents[tokens[i]]) != procident.Same {
			c.t.Fatalf("the harness of %s is not the one from before the cut: %+v", run, got)
		}
	}
}
