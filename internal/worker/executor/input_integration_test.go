//go:build integration && (linux || darwin)

package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// inputFixture is a fakeFixture whose harness takes structured input and whose
// Worker announces it. The permission bridge is the fakeharness itself, which
// asks the socket of the Run as `cf worker mcp-bridge` does.
func newInputFixture(t *testing.T, steps ...fakeharness.Step) *fakeFixture {
	t.Helper()
	f := newFakeFixture(t, 2*time.Second, every(configWith(t, "dark", steps...)))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A unix socket path is short: the temp directory of a test is not.
	dir, err := os.MkdirTemp("/tmp", "cfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fake := fakeharness.Adapter{Binary: self, Env: []string{fakeharness.AsFakeharnessEnv + "=1"}, Caps: harness.Capabilities{
		Modes: []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}, ConfigIsolation: true,
		StructuredInput: true, NeedsInputSignal: true,
	}}
	f.opts.Harnesses = registry(t, fake)
	f.opts.StructuredInput = []string{fakeharness.DefaultID}
	f.opts.SocketDir, f.opts.PermissionServerArgv = dir, []string{"/bin/true"}
	f.e = f.build()
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	return f
}

func askSpec(permissions string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"schema": 2, "prompt": "do it", "mode": "headless",
		"requirements": map[string]any{"harness": fakeharness.DefaultID, "permissions": permissions},
	})
	return raw
}

func (f *fakeFixture) requested(k state.Key) v1.RunReportEntry {
	f.t.Helper()
	var got v1.RunReportEntry
	waitFor(f.t, "input_requested", func() bool {
		for _, r := range f.reports(k) {
			if r.Kind == v1.ReportInputRequested {
				got = r
				return true
			}
		}
		return false
	})
	return got
}

// answer is the `input` order of the Controller for a prompt of k.
func (f *fakeFixture) answer(k state.Key, seq uint64, promptID string, kind domain.PromptKind, a v1.InputAnswer) {
	f.t.Helper()
	r := f.run(k)
	blocked, err := f.e.persist(f.t.Context(), []v1.WorkerCommand{{Seq: seq, Type: domain.CommandInput, Input: &v1.InputCommand{
		RunID: k.RunID, LeaseID: r.LeaseID, Epoch: k.LeaseEpoch, PromptID: promptID, Kind: kind, Answer: a,
	}}})
	if err != nil || blocked {
		f.t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	f.e.kickDecider()
}

func (f *fakeFixture) fileOfRun(k state.Key, name string) string {
	f.t.Helper()
	var got string
	waitFor(f.t, name, func() bool {
		raw, err := os.ReadFile(filepath.Join(f.worktree(k), name)) //nolint:gosec // a file of the test
		got = string(raw)
		return err == nil
	})
	return got
}

func permissionStep(to string) fakeharness.Step {
	return fakeharness.Step{Permission: &fakeharness.Permission{
		Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`), To: to, TimeoutMS: 30000,
	}}
}

func delivered(rs []v1.RunReportEntry, promptID string) bool {
	return slices.ContainsFunc(rs, func(r v1.RunReportEntry) bool {
		return r.Kind == v1.ReportInputDelivered && r.PromptID == promptID
	})
}

// Given a Run that asks permissions, When its harness asks to use a tool,
// Then the Worker reports input_requested with a summary of the call.
func TestScenario_NeedsInputInbox_Permiso(t *testing.T) {
	f := newInputFixture(t, permissionStep("decision.txt"))
	k := f.assign(askSpec(v1.PermissionsAsk))
	defer f.runLoops()()

	req := f.requested(k)
	if req.PromptKind != domain.PromptPermission || !strings.Contains(req.Summary, "Bash") || !strings.Contains(req.Summary, `"command":"ls"`) {
		t.Errorf("input_requested = %+v", req)
	}
	if !slices.Equal(req.Options, []string{domain.AnswerAllow, domain.AnswerDeny}) {
		t.Errorf("options = %v", req.Options)
	}
	if n := countKind(f.reports(k), v1.ReportInputRequested); n != 1 {
		t.Errorf("%d input_requested reports", n)
	}
}

// Given a prompt waiting, When the Controller orders the answer, Then the
// harness gets the decision, the Worker reports input_delivered and the Run ends.
func TestScenario_NeedsInputInbox_Respuesta(t *testing.T) {
	f := newInputFixture(t, permissionStep("decision.txt"))
	k := f.assign(askSpec(v1.PermissionsAsk))
	defer f.runLoops()()

	req := f.requested(k)
	f.answer(k, 1000, req.PromptID, domain.PromptPermission, v1.InputAnswer{Decision: domain.AnswerAllow})

	if got := f.fileOfRun(k, "decision.txt"); got != domain.AnswerAllow {
		t.Errorf("the harness got %q", got)
	}
	f.waitPhase(k, state.PhaseStopped)
	if rs := f.reports(k); !delivered(rs, req.PromptID) {
		t.Errorf("no input_delivered in %v", kinds(rs))
	}
}

// Given a Run in waiting_input, When the daemon restarts, Then the new daemon
// makes the socket again, the harness asks again, and the answer reaches it.
func TestScenario_NeedsInputInbox_Reinicio(t *testing.T) {
	f := newInputFixture(t, permissionStep("decision.txt"))
	k := f.assign(askSpec(v1.PermissionsAsk))
	stop := f.runLoops()
	req := f.requested(k)
	stop()
	before := f.wrapperOf(k)

	f.restart("01J0000000000000000000000E2")
	defer f.runLoops()()
	f.answer(k, 1000, req.PromptID, domain.PromptPermission, v1.InputAnswer{Decision: domain.AnswerDeny})

	if got := f.fileOfRun(k, "decision.txt"); got != domain.AnswerDeny {
		t.Errorf("the harness got %q", got)
	}
	f.waitPhase(k, state.PhaseStopped)
	rs := f.reports(k)
	if !delivered(rs, req.PromptID) || countKind(rs, v1.ReportInputRequested) != 1 {
		t.Errorf("reports = %v", kinds(rs))
	}
	if after := f.wrapperOf(k); after.PID != before.PID || after.Start != before.Start || countKind(rs, v1.ReportStarted) != 1 {
		t.Errorf("the harness was launched again: wrapper %+v, then %+v; reports %v", before, after, kinds(rs))
	}
}

// A question of the harness is answered through its input stream.
func TestScenario_NeedsInputInbox_PreguntaPorFIFO(t *testing.T) {
	f := newInputFixture(t,
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventInputRequired, ID: "q1", Message: "which?", Options: []string{"a", "b"}}},
		fakeharness.Step{ReadLine: &fakeharness.ReadLine{TimeoutMS: 30000, To: "answer.txt"}},
	)
	k := f.assign(askSpec(""))
	defer f.runLoops()()

	req := f.requested(k)
	if req.PromptKind != domain.PromptQuestion || req.PromptID != "q-q1" || !slices.Equal(req.Options, []string{"a", "b"}) {
		t.Fatalf("input_requested = %+v", req)
	}
	f.answer(k, 1000, req.PromptID, domain.PromptQuestion, v1.InputAnswer{Text: "b"})

	if got := f.fileOfRun(k, "answer.txt"); got != "b" {
		t.Errorf("the harness read %q", got)
	}
	f.waitPhase(k, state.PhaseStopped)
	if !delivered(f.reports(k), req.PromptID) {
		t.Errorf("no input_delivered in %v", kinds(f.reports(k)))
	}
	// The question was answered and the harness exited 0: nothing is left to ask.
	var exited *v1.RunReportEntry
	for _, r := range f.reports(k) {
		if r.Kind == v1.ReportExited {
			exited = &r
		}
	}
	if exited == nil || exited.EndReason != "" || exited.ExitCode == nil || *exited.ExitCode != 0 {
		t.Errorf("exited report = %+v, want a clean exit with no end reason", exited)
	}
}

func countKind(rs []v1.RunReportEntry, k v1.RunReportKind) int {
	n := 0
	for _, r := range rs {
		if r.Kind == k {
			n++
		}
	}
	return n
}
