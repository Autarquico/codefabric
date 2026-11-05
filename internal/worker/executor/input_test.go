package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/inputsock"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The summary and the options of a prompt leave the Worker without the secrets
// of the Run: the Controller has none to redact them with.
func TestOpenPromptRedactsTheSummaryAndTheOptionsWithTheSecretsOfTheRun(t *testing.T) {
	f := newFixture(t)
	k := f.started("ask")
	const secret = "s3cr3t-value-9f2c"
	if err := f.e.jit.Deliver(f.runDir(k), 1, []redact.Needle{{Ref: "github.personal", Value: []byte(secret)}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	err := f.e.openPrompt(t.Context(), f.run(k), "q-1", domain.PromptQuestion, "use "+secret+"?", []string{secret, "no"}, true)
	if err != nil {
		t.Fatal(err)
	}
	reports := f.reports(k)
	got := reports[len(reports)-1]
	if got.Kind != v1.ReportInputRequested || strings.Contains(got.Summary, secret) || slices.ContainsFunc(got.Options, func(o string) bool { return strings.Contains(o, secret) }) {
		t.Errorf("input_requested = %+v", got)
	}
	if len(got.Options) != 2 || got.Options[1] != "no" {
		t.Errorf("options = %v", got.Options)
	}
}

// Text that cannot be redacted is withheld, never reported as it is.
func TestOpenPromptWithholdsWhatItCannotRedact(t *testing.T) {
	f := newFixture(t)
	k := f.started("ask")
	if err := f.e.jit.Deliver(f.runDir(k), 1, []redact.Needle{{Ref: "github.personal", Value: []byte("s3cr3t-value-9f2c")}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := corruptDelivery(f.runDir(k)); err != nil {
		t.Skipf("cannot corrupt the delivery: %v", err)
	}
	if err := f.e.openPrompt(t.Context(), f.run(k), "q-1", domain.PromptQuestion, "s3cr3t-value-9f2c", nil, true); err != nil {
		t.Fatal(err)
	}
	reports := f.reports(k)
	if got := reports[len(reports)-1].Summary; got != withheld {
		t.Errorf("summary = %q", got)
	}
}

// An order for a Run that is not up here, or under another lease, is dropped:
// the cursor moves and nothing is queued.
func TestAnInputOrderForARunThatIsNotUpIsIgnored(t *testing.T) {
	f := newFixture(t)
	accepted := f.accepted("not started") // not running: nobody to answer
	running := f.started("running")
	ghost := state.Key{RunID: runID(9999), LeaseEpoch: 1}

	order := func(seq uint64, k state.Key, lease string) v1.WorkerCommand {
		return v1.WorkerCommand{Seq: seq, Type: domain.CommandInput, Input: &v1.InputCommand{
			RunID: k.RunID, LeaseID: lease, Epoch: k.LeaseEpoch, PromptID: "p1", Kind: domain.PromptPermission,
			Answer: v1.InputAnswer{Decision: domain.AnswerAllow},
		}}
	}
	cmds := []v1.WorkerCommand{
		order(5001, accepted, f.run(accepted).LeaseID),
		order(5002, ghost, "lease_x"),
		order(5003, running, "another-lease"),
	}
	blocked, err := f.e.persist(t.Context(), cmds)
	if err != nil || blocked {
		t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	for _, k := range []state.Key{accepted, running, ghost} {
		orders, err := state.PendingInputOrders(t.Context(), f.db.Reader(), k)
		if err != nil || len(orders) != 0 {
			t.Errorf("%s: orders = %v (%v)", k.RunID, orders, err)
		}
	}
}

// corruptDelivery makes the secrets of the run directory unreadable.
func corruptDelivery(runDir string) error {
	return os.WriteFile(filepath.Join(runDir, "secrets", "manifest.json"), []byte("{"), 0o600)
}

// socketDir is a directory whose sockets fit: t.TempDir() is too long on macOS.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// askSocket is a started Run with the socket of its bridge up.
func askSocket(t *testing.T) (*fixture, state.Key, *inputsock.Client) {
	t.Helper()
	dir := socketDir(t)
	f := newFixture(t, func(o *Options) { o.SocketDir = dir })
	k := f.started("ask")
	f.e.ensureSocket(t.Context(), f.run(k))
	if f.e.inputOf(k) == nil {
		t.Fatal("the socket of the Run is not up")
	}
	t.Cleanup(f.e.closeInputs)
	path, err := inputsock.Path(dir, k.RunID, k.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return f, k, &inputsock.Client{Path: path, Wait: 10 * time.Millisecond}
}

// The supervisor takes the socket of a Run down under the lock of the Run, and a
// question of the bridge may be waiting for that lock: the supervisor does not
// wait for it.
func TestTakingTheSocketDownUnderTheLockOfTheRunDoesNotWaitForItsHandlers(t *testing.T) {
	f, k, client := askSocket(t)
	unlock := f.e.lock(k)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, _ = client.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	}()
	waitFor(t, "a handler waiting for the lock", func() bool {
		f.e.mu.Lock()
		defer f.e.mu.Unlock()
		l := f.e.locks[k]
		return l != nil && l.refs == 2
	})
	done := make(chan struct{})
	go func() {
		f.e.dropInput(t.Context(), k)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("taking the socket down waited for a handler that waits for the lock it holds")
	}
	unlock()
	cancel()
}
