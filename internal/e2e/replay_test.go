//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/e2e/netproxy"
)

// replayLines is how many lines the harness prints, a quarter of a second apart, all before the partition ends (a line printed after the harness exits is not captured).
const replayLines = 12

// replayPartition is how long the Worker is cut off: well under run_lost_timeout
// (8 s before CF_E2E_SCALE), so that the Run is never lost and what comes back
// is a replay, not a reconciliation.
const replayPartition = 4 * time.Second

var replayLine = regexp.MustCompile(`(?m)^replay line (\d+)$`)

// TestScenario_JournalDegradation_ReplayE2e (Replay, e2e #4, short): a Worker
// cut off while its harness prints and finishes. When the network comes back
// its events, logs and output arrive, none twice and in the order of their
// worker_seq within the Run. (The 10 minutes of the scenario are simulated in
// internal/worker/journal; this one runs the real daemons.)
func TestScenario_JournalDegradation_ReplayE2e(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1, Proxied: true})
	sampler := c.StartSampler()
	token := c.Ledger.Token()
	emitter := fmt.Sprintf(`( i=0; while [ $i -lt %d ]; do i=$((i+1)); echo "replay line $i"; sleep 0.25; done ) &
`, replayLines)
	created := c.Do(emitter + c.Ledger.Script(token))
	run := created.Run.ID
	in := waitHarness(c, token)
	w := workerNamed(c, in.Worker)

	c.Eventually("the first lines of the output to reach the Controller", func() (bool, string) {
		out := replayedLines(c, run)
		return len(out) >= 3, fmt.Sprint(out)
	})

	w.Proxy.SetMode(netproxy.Reject)
	time.Sleep(Scaled(replayPartition))
	c.Ledger.Release(token) // the harness ends while nobody hears of it
	c.Eventually("the harness to end", func() (bool, string) {
		got := c.Ledger.Instances(token)
		return len(got) == 1 && got[0].Ended, fmt.Sprintf("%+v", got)
	})
	if got := replayedLines(c, run); len(got) >= replayLines {
		t.Fatalf("the Controller has all %d lines while the Worker is cut off", len(got))
	}
	if r := c.Run(c.Observer, run); r.State != domain.RunRunning {
		t.Fatalf("the Run is %s during the partition: it was to be running, not lost", RunStage(r))
	}

	w.Proxy.Heal()
	if r := waitPublished(c, run); r.State != domain.RunCompleted {
		t.Fatalf("Run = %s", RunStage(r))
	}

	// Output: every line, once, in order.
	c.Eventually("all the output to arrive", func() (bool, string) {
		got := replayedLines(c, run)
		return len(got) == replayLines, fmt.Sprint(got)
	})
	want := make([]int, replayLines)
	for i := range want {
		want[i] = i + 1
	}
	if got := replayedLines(c, run); !slices.Equal(got, want) {
		t.Errorf("output lines = %v, want %v", got, want)
	}
	if out := c.mustCF(c.Observer, "logs", "--run", run).Stdout; !strings.Contains(out, "e2e: finished "+token) {
		t.Errorf("the end of the output did not arrive:\n%s", out)
	}

	// Events: the Run's own, once each, in worker_seq order.
	snap := c.ControllerSnapshot()
	for _, typ := range []domain.EventType{domain.EventRunStarted, domain.EventRunCompleted} {
		if n := len(eventsOf(snap, run, typ)); n != 1 {
			t.Errorf("%d %s events, want 1", n, typ)
		}
	}
	var triples []string
	perEpoch := map[string]int64{}
	for _, e := range snap.Events {
		if e.RunID != run || e.WorkerSeq == nil {
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", e.WorkerID, e.WorkerEpoch, *e.WorkerSeq)
		if slices.Contains(triples, key) {
			t.Errorf("event %s twice", key)
		}
		triples = append(triples, key)
		if *e.WorkerSeq <= perEpoch[e.WorkerEpoch] {
			t.Errorf("event %s (sequence %d) is not after worker_seq %d of its epoch", key, e.Sequence, perEpoch[e.WorkerEpoch])
		}
		perEpoch[e.WorkerEpoch] = *e.WorkerSeq
	}
	if len(triples) == 0 {
		t.Error("no event of the Run carries a worker_seq")
	}

	// Structured logs of the Run: none twice, in worker_seq order.
	var logs []v1.Log
	c.Eventually("the structured logs of the Run to arrive", func() (bool, string) {
		logs = runLogs(c, run)
		return len(logs) > 0, "none yet"
	})
	last := map[string]uint64{}
	for _, l := range logs {
		if l.WorkerSeq == nil {
			continue
		}
		if *l.WorkerSeq <= last[l.WorkerEpoch] {
			t.Errorf("log %q has worker_seq %d after %d: repeated or out of order", l.Message, *l.WorkerSeq, last[l.WorkerEpoch])
		}
		last[l.WorkerEpoch] = *l.WorkerSeq
	}

	if n := c.Bypasses(); n != 0 {
		t.Errorf("%d connections went around the proxies", n)
	}
	Check(t, c, Expect{Tasks: []ExpectedTask{{ID: created.Task.ID, Token: token}}, Sampler: sampler})
}

// replayedLines are the numbers of the "replay line N" lines of a Run's output
// that the Controller has, in the order it shows them.
func replayedLines(c *Cluster, run string) []int {
	c.t.Helper()
	var out []int
	for _, m := range replayLine.FindAllStringSubmatch(c.mustCF(c.Observer, "logs", "--run", run, "--tail", "1000").Stdout, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// runLogs are all the structured logs of a Run, oldest first.
func runLogs(c *Cluster, run string) []v1.Log {
	c.t.Helper()
	var all []v1.Log
	after := ""
	for {
		page, err := c.API.Logs(c.H.Context(), client.LogQuery{Run: run, After: after, Limit: 500})
		if err != nil {
			c.t.Fatal(err)
		}
		all = append(all, page.Logs...)
		if !page.HasMore {
			return all
		}
		after = page.Cursor
	}
}
