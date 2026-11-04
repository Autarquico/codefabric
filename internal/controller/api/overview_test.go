package api_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestOverview_CountsAndCursorComeFromOneSnapshot(t *testing.T) {
	tc := newController(t, time.Now())
	newTasks(t, tc, 3, "t")
	must(t, tc.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE tasks SET state = 'waiting', waiting_reason = 'input' WHERE name = 't-0'`)
		return err
	}))

	got, err := tc.owner.Overview(t.Context())
	must(t, err)

	if want := strconv.FormatUint(lastSequence(t, tc), 10); got.Cursor != want {
		t.Errorf("cursor = %s, want the highest sequence stored, %s", got.Cursor, want)
	}
	total := 0
	for _, n := range got.Tasks {
		total += n
	}
	if total != 3 || got.NeedsInput != 1 {
		t.Errorf("tasks %+v, needs input %d", got.Tasks, got.NeedsInput)
	}
	if len(got.Activity) == 0 || got.Activity[len(got.Activity)-1].Sequence != lastSequence(t, tc) {
		t.Errorf("activity does not end at the cursor: %+v", got.Activity)
	}
	if got.Runs == nil || got.Workers == nil || got.WorkerHealth == nil {
		t.Errorf("a missing map would decode as nil: %+v", got)
	}
}

func TestOverview_CountsWorkersByStateAndHealth(t *testing.T) {
	tc := newController(t, time.Now())
	tc.enrollRealWorker("w1")

	got, err := tc.owner.Overview(t.Context())
	must(t, err)
	if got.Workers[domain.WorkerJoining] != 1 || got.WorkerHealth[v1.WorkerHealthUnknown] != 1 {
		t.Errorf("workers %+v, health %+v", got.Workers, got.WorkerHealth)
	}
}

func TestOverview_ActivityIsBoundedAndWorkersMayNotAsk(t *testing.T) {
	tc := newController(t, time.Now())
	newTasks(t, tc, 3, "t")
	hc := tc.httpClientFor(tc.ownerDir)

	if status, env, _ := tc.call(hc, http.MethodGet, "/v1/overview?activity=101", ""); status != http.StatusBadRequest {
		t.Errorf("activity=101: %d %+v", status, env)
	}
	if status, _, _ := tc.call(hc, http.MethodGet, "/v1/overview?activity=x", ""); status != http.StatusBadRequest {
		t.Errorf("activity=x: %d", status)
	}
	if status, _, _ := tc.call(tc.enrollWorker(), http.MethodGet, "/v1/overview", ""); status != http.StatusForbidden {
		t.Errorf("a Worker asked: %d", status)
	}
}
