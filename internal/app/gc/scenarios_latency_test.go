package gc_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/app/gc"
)

// Each batch is its own transaction: when AfterBatch runs, exactly the rows the
// batch reported are gone for every other connection, and no more than
// gc.batch_size of them. A pass that deleted in one transaction would show the
// whole table still there until its end.
func TestScenario_RetentionGc_LatenciaEachBatchIsCommittedOnItsOwn(t *testing.T) {
	const batch = 7
	f := newFixture(t)
	for range 40 {
		f.event("")
	}
	f.clk.Advance(10 * day)
	f.event("")

	left, batches := f.count("events"), 0
	hooks := gc.Hooks{AfterBatch: func(kind string, rows int64) {
		if kind != "events" {
			return
		}
		batches++
		now := f.count("events")
		if rows > batch || left-now != int(rows) {
			t.Errorf("batch %d: reported %d rows (limit %d), %d left the table", batches, rows, batch, left-now)
		}
		left = now
	}}
	policy := shortPolicy
	policy.BatchSize = batch
	if _, err := f.service(policy, gc.WithHooks(hooks)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if want := (40 + batch - 1) / batch; batches != want || left != 1 {
		t.Fatalf("%d batches, %d events left; want %d batches and the recent event", batches, left, want)
	}
}
