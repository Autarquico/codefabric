package cli

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestPrintExplanation_QueuedRun(t *testing.T) {
	var out strings.Builder
	err := printExplanation(&out, v1.RunExplanation{
		Short: "run-42", State: domain.RunQueued, QueuedReason: "CF_NO_ELIGIBLE_WORKER",
		Requested: v1.Resources{CPU: 2, MemoryMB: 4096}, Decision: v1.DecisionNoWorker,
		Workers: []v1.WorkerVerdict{
			{Name: "alpha", State: domain.WorkerOnline, Failed: []v1.FilterResult{{Filter: "capabilities", Message: "missing capabilities: gpu"}}},
			{Name: "beta", State: domain.WorkerOnline, Eligible: true, Chosen: true, Score: &v1.ScoreBreakdown{Total: 1100, Free: 1000, Affinity: 100}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Run run-42 (queued) reason CF_NO_ELIGIBLE_WORKER · requests cpu 2, memory 4.0 GB",
		"alpha", "rejected", "capabilities: missing capabilities: gpu",
		"beta", "chosen", "score 1100 (prefer 0, free 1000, load 0, affinity 100)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPrintExplanation_OtherDecisions(t *testing.T) {
	var out strings.Builder
	must := func(e v1.RunExplanation) {
		t.Helper()
		if err := printExplanation(&out, e); err != nil {
			t.Fatal(err)
		}
	}
	must(v1.RunExplanation{
		Short: "run-1", State: domain.RunQueued, Decision: v1.DecisionAssigned, Chosen: "alpha",
		Lease: &v1.Lease{ID: "lease_x", Epoch: 2, State: domain.LeasePending},
	})
	must(v1.RunExplanation{Short: "run-2", State: domain.RunCancelled, Decision: v1.DecisionNotQueued})
	must(v1.RunExplanation{Short: "run-3", State: domain.RunQueued, Decision: v1.DecisionNoWorker})
	for _, want := range []string{"Assigned to alpha under lease lease_x, epoch 2 (pending).", "not waiting for a Worker", "There are no Workers."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
