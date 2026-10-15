//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// clusterUpLimit is how long `cf do` may take to a published result.
const clusterUpLimit = 20 * time.Second

func TestHarness_ClusterUp(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1, Proxied: true})
	w := c.Workers[0]
	token := c.Ledger.Token()

	began := time.Now()
	created := c.Do(c.Ledger.Script(token))
	run := created.Run.ID
	var started Instance
	c.Eventually("the harness to start", func() (bool, string) {
		got := c.Ledger.Instances(token)
		if len(got) == 1 && c.Ledger.Live(got[0]) {
			started = got[0]
			return true, ""
		}
		r, _ := c.API.Run(c.H.Context(), run)
		return false, RunStage(r)
	})
	c.Ledger.Release(token)
	r := c.WaitRun(run, "completed and published", func(r v1.Run) bool {
		return r.State == domain.RunCompleted && r.Publication == domain.PublicationPublished
	})
	if took := time.Since(began); took > Scaled(clusterUpLimit) {
		t.Errorf("cf do to published took %v, more than %v", took.Round(time.Millisecond), Scaled(clusterUpLimit))
	}

	// The result is in the published commit.
	{
		if got := gittest.Run(t, c.RelayRepo(), "show", r.ResultCommit+":result.txt"); got != token {
			t.Errorf("result.txt = %q, want %q", got, token)
		}
		if tip := gittest.Run(t, c.RelayRepo(), "rev-parse", "refs/heads/"+r.Branch); tip != r.ResultCommit {
			t.Errorf("relay tip %s, want %s", tip, r.ResultCommit)
		}
	}

	// The ledger names one execution, by the wrapper.
	{
		want := Execution{Run: run, Epoch: 1, Worker: w.Name}
		if started.Execution != want || !started.Verified {
			t.Errorf("started = %+v, want %v verified", started, want)
		}
		c.Eventually("the harness to be gone", func() (bool, string) {
			got := c.Ledger.Instances(token)
			return len(got) == 1 && got[0].Ended && !c.Ledger.Live(got[0]), fmt.Sprintf("%+v", got)
		})
		if all := c.Ledger.All(); len(all) != 1 {
			t.Errorf("the ledger has %d instances", len(all))
		}
	}

	// The Worker reached the Controller through its proxy only.
	{
		if s := w.Proxy.Stats(); s.Accepted == 0 {
			t.Errorf("the proxy accepted nothing: %+v", s)
		}
		if n := c.Bypasses(); n != 0 {
			t.Errorf("%d connections went to the address stored at join", n)
		}
	}

	// The databases read back the Run.
	{
		c.Eventually("the reservation to be released", func() (bool, string) {
			open := c.ControllerSnapshot().OpenReservations
			return len(open) == 0, fmt.Sprintf("open reservations: %+v", open)
		})
		snap := c.ControllerSnapshot()
		var fromWorker int
		for _, e := range snap.Events {
			if e.RunID == run && e.Type == domain.EventRunStarted {
				if e.WorkerID != w.ID || e.WorkerEpoch == "" || e.WorkerSeq == nil {
					t.Errorf("RunStarted without the triple of the Worker: %+v", e)
				}
				fromWorker++
			}
		}
		if fromWorker != 1 {
			t.Errorf("%d RunStarted events", fromWorker)
		}
		if n := countFunc(snap.Leases, func(l LeaseRow) bool { return l.RunID == run && l.WorkerID == w.ID && l.Epoch == 1 }); n != 1 || len(snap.Leases) != 1 {
			t.Errorf("leases = %+v", snap.Leases)
		}
		if n := countFunc(snap.Receipts, func(r ReceiptRow) bool { return r.RunID == run && r.WorkerID == w.ID }); n == 0 {
			t.Error("no report receipts of the Run")
		}
		if len(snap.Orphans) != 0 {
			t.Errorf("orphans = %+v", snap.Orphans)
		}
		ws := c.WorkerSnapshot(w)
		var maxSeq int64
		for _, rc := range snap.Receipts {
			maxSeq = max(maxSeq, rc.WorkerSeq)
		}
		if ws.JournalSeq < maxSeq || ws.JournalSeq == 0 {
			t.Errorf("journal high-water mark %d, highest seq received %d", ws.JournalSeq, maxSeq)
		}
		if !slices.ContainsFunc(ws.LocalRuns, func(l LocalRunRow) bool { return l.RunID == run && l.LeaseEpoch == 1 }) {
			t.Errorf("local_runs = %+v", ws.LocalRuns)
		}
	}

	// The observer sees the Run, its events and its output.
	{
		if got := c.Run(c.Observer, created.Run.Short); got.ID != run || got.State != domain.RunCompleted {
			t.Errorf("run show = %+v", got)
		}
		events := c.Events(c.Observer, "--run", run)
		types := map[domain.EventType]int{}
		for _, e := range events {
			types[e.Type]++
		}
		if types[domain.EventRunStarted] != 1 || types[domain.EventRunPublished] != 1 {
			t.Errorf("events of the Run: %v", types)
		}
		logs := c.CF(c.Observer, "logs", "--run", run)
		if logs.Code != 0 || !strings.Contains(logs.Stdout, "e2e: started "+token) || !strings.Contains(logs.Stdout, "e2e: finished "+token) {
			t.Errorf("%s", logs)
		}
		if ws := c.ListWorkers(c.Observer); len(ws) != 1 || ws[0].Name != w.Name {
			t.Errorf("workers = %+v", ws)
		}
		if !strings.Contains(c.RunShow(c.Observer, run), "State        completed") {
			t.Error("run show does not say completed")
		}
		Fails(c, c.Observer, domain.CodeNotFound, "run", "show", "r-nothing")
	}

	// The Controller comes back on its port after a kill -9, and the Worker
	// after its own, through the proxy still.
	{
		c.Controller.Kill9()
		c.RestartController()
		c.RestartWorker(w)
		if n := c.Bypasses(); n != 0 {
			t.Errorf("%d connections went to the address stored at join", n)
		}
	}
}

func countFunc[T any](s []T, f func(T) bool) int {
	n := 0
	for _, v := range s {
		if f(v) {
			n++
		}
	}
	return n
}
