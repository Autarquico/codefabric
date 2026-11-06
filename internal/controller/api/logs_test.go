package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestLogs_AWorkerShipsAndAClientReadsAndFollowsThroughTheSDK(t *testing.T) {
	tc := newController(t, time.Now())
	worker, _ := tc.enrollRealWorker("alpha")
	ctx := t.Context()
	line := func(seq uint64, level, msg string) v1.LogEntry {
		return v1.LogEntry{WorkerEpoch: "E1", WorkerSeq: seq, Timestamp: time.Now(), Level: level, Message: msg, Attrs: []byte(`{"n":1}`)}
	}
	got, err := worker.ShipLogs(ctx, v1.LogBatch{Entries: []v1.LogEntry{line(1, "info", "one"), line(2, "error", "two")}})
	if err != nil || got != (v1.LogBatchResponse{Applied: 2}) {
		t.Fatalf("ship = %+v, %v", got, err)
	}
	page, err := tc.owner.Logs(ctx, client.LogQuery{Worker: "alpha", Level: "warn"})
	if err != nil || len(page.Logs) != 1 || page.Logs[0].Message != "two" || string(page.Logs[0].Attrs) != `{"n":1}` {
		t.Fatalf("page = %+v, %v", page, err)
	}

	followCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	followed := make(chan v1.Log, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- tc.owner.FollowLogs(followCtx, client.LogQuery{Worker: "alpha"}, func(l v1.Log) error {
			followed <- l
			return errors.New("enough")
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for seq := uint64(3); ; seq++ {
		if _, err := worker.ShipLogs(ctx, v1.LogBatch{Entries: []v1.LogEntry{line(seq, "info", "live")}}); err != nil {
			t.Fatal(err)
		}
		select {
		case l := <-followed:
			if l.Message != "live" || l.Sequence <= page.Logs[0].Sequence {
				t.Fatalf("followed %+v", l)
			}
			if err := <-errc; err == nil || err.Error() != "enough" {
				t.Fatalf("follow ended with %v", err)
			}
			return
		case <-time.After(200 * time.Millisecond): // the stream may not be open yet: "from now" was before it
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing followed")
		}
	}
}

func TestLogs_OnlyWorkersShipAndOnlyClientsRead(t *testing.T) {
	tc := newController(t, time.Now())
	worker, workerDir := tc.enrollRealWorker("alpha")
	if _, err := tc.owner.ShipLogs(t.Context(), v1.LogBatch{}); !hasCode(err, domain.CodePolicyDenied) {
		t.Fatalf("a client ships: %v", err)
	}
	if _, err := worker.Logs(t.Context(), client.LogQuery{}); !hasCode(err, domain.CodePolicyDenied) {
		t.Fatalf("a Worker reads: %v", err)
	}
	status, body, _ := tc.call(tc.httpClientFor(workerDir), http.MethodPost, "/v1/worker/logs", `{"entries":[`+
		`{"worker_epoch":"E","worker_seq":1,"ts":"2026-10-10T12:00:00Z","level":"info","msg":"x"}`+
		`]}`)
	if status != http.StatusOK {
		t.Fatalf("POST = %d %+v", status, body)
	}
	status, body, _ = tc.call(tc.httpClientFor(tc.ownerDir), http.MethodGet, "/v1/logs?after=9999", "")
	if status != http.StatusConflict || body.Code != domain.CodeCursorExpired {
		t.Fatalf("a cursor ahead = %d %+v", status, body)
	}
}

func hasCode(err error, code domain.Code) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}

// With the stream open, one line reaches the client in under 2 s.
func TestLogs_ALineReachesAnOpenStreamWithinTwoSeconds(t *testing.T) {
	tc := newController(t, time.Now())
	worker, _ := tc.enrollRealWorker("alpha")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lines := make(chan v1.Log, 4)
	errc := make(chan error, 1)
	go func() {
		errc <- tc.owner.FollowLogs(ctx, client.LogQuery{Worker: "alpha"}, func(l v1.Log) error {
			lines <- l
			return nil
		})
	}()
	// The stream is open once a line shipped after it arrives; those that come
	// before it are "before now" and skipped. Then the one that is timed.
	for seq, opened := uint64(1), false; !opened; seq++ {
		if _, err := worker.ShipLogs(ctx, v1.LogBatch{Entries: []v1.LogEntry{{WorkerEpoch: "E1", WorkerSeq: seq, Timestamp: time.Now(), Level: "info", Message: "probe"}}}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-lines:
			opened = true
		case <-time.After(200 * time.Millisecond):
		case err := <-errc:
			t.Fatalf("follow ended: %v", err)
		}
	}
	sent := time.Now()
	if _, err := worker.ShipLogs(ctx, v1.LogBatch{Entries: []v1.LogEntry{{WorkerEpoch: "E1", WorkerSeq: 1000, Timestamp: time.Now(), Level: "info", Message: "timed"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case l := <-lines:
			if l.Message != "timed" {
				continue // a probe still in flight
			}
			if took := time.Since(sent); took > 2*time.Second {
				t.Fatalf("the line took %v", took)
			}
			return
		case <-time.After(2 * time.Second):
			t.Fatal("the line did not arrive in 2 s")
		}
	}
}
