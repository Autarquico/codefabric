//go:build linux || darwin

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The values of the secrets of the tests: what must never be found where
// only refs belong.
const (
	ghValue  = "ghp-sentinel-value-0123456789"
	apiValue = "api-sentinel-value-9876543210"
)

// fakeStore is a catalog of secrets in memory that counts the values read.
type fakeStore struct {
	mu      sync.Mutex
	entries map[string]fakeSecret
	gets    []string
	listErr error
	// onGet, if set, runs before a value is read.
	onGet func(ref string)
}

type fakeSecret struct {
	value    []byte
	projects []string
	backend  string
}

func newFakeStore(values map[string]string) *fakeStore {
	s := &fakeStore{entries: map[string]fakeSecret{}}
	for ref, v := range values {
		s.entries[ref] = fakeSecret{value: []byte(v), backend: secrets.BackendFile}
	}
	return s
}

func (s *fakeStore) List(context.Context) ([]secrets.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []secrets.Metadata
	for ref, e := range s.entries {
		out = append(out, secrets.Metadata{Ref: ref, Backend: e.backend, Projects: e.projects})
	}
	slices.SortFunc(out, func(a, b secrets.Metadata) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

func (s *fakeStore) Get(_ context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	hook := s.onGet
	s.mu.Unlock()
	if hook != nil {
		hook(ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, ref)
	e, ok := s.entries[ref]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return bytes.Clone(e.value), nil
}

func (s *fakeStore) Set(_ context.Context, ref string, value []byte, projects []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[ref] = fakeSecret{value: bytes.Clone(value), projects: projects, backend: secrets.BackendFile}
	return nil
}

func (s *fakeStore) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, ref)
	return nil
}

func (s *fakeStore) reads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.gets)
}

func (s *fakeStore) scope(ref string, projects ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[ref]
	e.projects = projects
	s.entries[ref] = e
}

// secretSpec is a spec of schema 3 for the command harness whose environment
// carries env (name to ref).
func secretSpec(prompt, mode string, env map[string]string) []byte {
	return secretSpecFor("command", prompt, mode, env)
}

// secretSpecFor is secretSpec for the harness harnessID.
func secretSpecFor(harnessID, prompt, mode string, env map[string]string) []byte {
	type pair struct {
		Name string `json:"name"`
		Ref  string `json:"ref"`
	}
	var pairs []pair
	var refs []string
	for name, ref := range env {
		pairs = append(pairs, pair{name, ref})
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(refs)
	raw, _ := json.Marshal(map[string]any{
		"schema": domain.RunSpecSchemaSecrets, "prompt": prompt, "mode": mode,
		"requirements": map[string]any{"harness": harnessID},
		"secrets":      map[string]any{"env": pairs, "refs": refs},
	})
	return raw
}

// secretFixture is a fixture whose Worker holds store.
func secretFixture(t *testing.T, store *fakeStore, mutate ...func(*Options)) *fixture {
	t.Helper()
	return newFixture(t, append([]func(*Options){func(o *Options) {
		o.Secrets = store
		o.RedactPipeArgv = []string{"/cf", "worker", "redact-pipe"}
	}}, mutate...)...)
}

// noValueIn fails if a value of the tests is in any file under dir, apart
// from the delivery itself.
func noValueIn(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && strings.HasPrefix(d.Name(), "secrets") {
				return filepath.SkipDir
			}
			return nil
		}
		raw, _ := os.ReadFile(path) //nolint:gosec // a file of the test
		for _, v := range []string{ghValue, apiValue} {
			if bytes.Contains(raw, []byte(v)) {
				t.Errorf("%s holds a value", path)
			}
		}
		return nil
	})
}

func noValueInReports(t *testing.T, reports []v1.RunReportEntry) {
	t.Helper()
	raw, _ := json.Marshal(reports)
	for _, v := range []string{ghValue, apiValue} {
		if bytes.Contains(raw, []byte(v)) {
			t.Errorf("a report holds a value: %s", raw)
		}
	}
}

func TestEvaluateTakesASpecWithSecretsTheWorkerHoldsForTheProject(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	store.scope("github.personal", "project_1")
	f := secretFixture(t, store)
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Fatalf("phase = %s", got)
	}
	if len(store.reads()) != 0 {
		t.Errorf("evaluating read values: %v", store.reads())
	}
}

func TestEvaluateRejectsWhatTheWorkerCannotHandOut(t *testing.T) {
	cases := []struct {
		name  string
		store func() *fakeStore
		spec  []byte
		code  domain.Code
		says  string
	}{
		{
			name: "a ref the catalog does not hold", store: func() *fakeStore { return newFakeStore(map[string]string{"other": ghValue}) },
			spec: secretSpec("x", "headless", map[string]string{"GH": "github.personal"}), code: domain.CodeSecretUnavailable, says: `"github.personal"`,
		},
		{
			name: "a ref held only for other Projects", store: func() *fakeStore {
				s := newFakeStore(map[string]string{"github.personal": ghValue})
				s.scope("github.personal", "project_9")
				return s
			},
			spec: secretSpec("x", "headless", map[string]string{"GH": "github.personal"}), code: domain.CodePolicyDenied, says: "only for other Projects",
		},
		{
			name: "a ref of a backend this Worker cannot read", store: func() *fakeStore {
				s := newFakeStore(nil)
				s.entries["github.personal"] = fakeSecret{value: []byte(ghValue), backend: "keyring"}
				return s
			},
			spec: secretSpec("x", "headless", map[string]string{"GH": "github.personal"}), code: domain.CodeSecretUnavailable, says: `"github.personal"`,
		},
		{
			name: "a catalog that cannot be read", store: func() *fakeStore {
				s := newFakeStore(nil)
				s.listErr = secrets.ErrUnsafe
				return s
			},
			spec: secretSpec("x", "headless", map[string]string{"GH": "github.personal"}), code: domain.CodeSecretUnavailable, says: "cannot be read",
		},
		{
			name: "a schema 3 that declares nothing", store: func() *fakeStore { return newFakeStore(nil) },
			spec: []byte(`{"schema":3,"prompt":"x","mode":"headless","requirements":{"harness":"command"}}`), code: domain.CodeVersionIncompatible, says: "declares no secret",
		},
		{
			name: "a schema 2 with secrets", store: func() *fakeStore { return newFakeStore(nil) },
			spec: []byte(`{"schema":2,"prompt":"x","mode":"headless","requirements":{"harness":"command"},"secrets":{"env":[],"refs":["a.b"]}}`), code: domain.CodeVersionIncompatible, says: "schema 2",
		},
		{
			name: "a variable of a ref the spec does not declare", store: func() *fakeStore { return newFakeStore(nil) },
			spec: []byte(`{"schema":3,"prompt":"x","mode":"headless","requirements":{"harness":"command"},"secrets":{"env":[{"name":"GH","ref":"x.y"}],"refs":["a.b"]}}`), code: domain.CodeVersionIncompatible, says: "does not declare",
		},
		{
			name: "a schema this Worker does not read", store: func() *fakeStore { return newFakeStore(nil) },
			spec: []byte(`{"schema":4,"prompt":"x","mode":"headless","requirements":{"harness":"command"}}`), code: domain.CodeVersionIncompatible, says: "schema 4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store()
			f := secretFixture(t, store)
			k := f.assign(tc.spec)
			if f.e.decideOnce(t.Context()) {
				t.Fatal("deciding wanted a retry")
			}
			if got := f.run(k).Phase; got != state.PhaseRejected {
				t.Fatalf("phase = %s, want rejected", got)
			}
			rej := f.ctrl.rejected
			if len(rej) != 1 || rej[0].Code != tc.code || !strings.Contains(rej[0].Message, tc.says) {
				t.Errorf("rejected = %+v, want %s saying %q", rej, tc.code, tc.says)
			}
			if !domain.ValidLeaseRejectCode(rej[0].Code) {
				t.Errorf("%s is no code a lease may be rejected with", rej[0].Code)
			}
			if len(store.reads()) != 0 {
				t.Errorf("values were read: %v", store.reads())
			}
		})
	}
}

func TestAWorkerWithoutACatalogRejectsARunWithSecrets(t *testing.T) {
	f := newFixture(t)
	f.assign(secretSpec("x", "headless", map[string]string{"GH": "github.personal"}))
	f.e.decideOnce(t.Context())
	if rej := f.ctrl.rejected; len(rej) != 1 || rej[0].Code != domain.CodeSecretUnavailable {
		t.Errorf("rejected = %+v", rej)
	}
}

func TestAGrantedRunGetsItsSecretsDeliveredAndLaunchJSONNamesOnlyRefs(t *testing.T) {
	for _, mode := range []string{"headless", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			store := newFakeStore(map[string]string{"github.personal": ghValue})
			f := secretFixture(t, store)
			k := f.acceptedSpec(secretSpec("echo hi", mode, map[string]string{"GH": "github.personal"}))
			if f.e.step(t.Context(), k) {
				t.Fatal("the Run ended at its first step")
			}
			if got := f.run(k).Phase; got != state.PhaseRunning {
				t.Fatalf("phase = %s, want running", got)
			}
			grants := f.ctrl.grantRequests()
			want := v1.GrantSecretsRequest{LeaseID: f.run(k).LeaseID, LeaseEpoch: 1, WorkerEpoch: testEpoch, Refs: []string{"github.personal"}}
			if len(grants) != 1 || !grantEqual(grants[0], want) {
				t.Errorf("grants = %+v, want %+v", grants, want)
			}
			if got := store.reads(); !slices.Equal(got, []string{"github.personal"}) {
				t.Errorf("values read = %v", got)
			}
			dir := f.runDir(k)
			if v, err := secretjit.Resolver(dir)("github.personal"); err != nil || v != ghValue {
				t.Errorf("the delivery resolves to %q, %v", v, err)
			}
			if got := f.run(k).PrepareAttempt; got != 1 {
				t.Errorf("prepare attempt = %d, want 1", got)
			}
			var l execwrap.Launch
			if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); !ok || err != nil {
				t.Fatalf("launch.json: %v %v", ok, err)
			}
			if !slices.Equal(l.SecretEnv, []execwrap.SecretEnv{{Name: "GH", Ref: "github.personal"}}) {
				t.Errorf("secret_env = %+v", l.SecretEnv)
			}
			noValueIn(t, dir)
			noValueIn(t, filepath.Dir(f.paths.WorkerStateFile()))
			noValueInReports(t, f.reports(k))

			spec := f.rt.starts[0]
			if mode == "interactive" {
				if want := []string{"/cf", "worker", "redact-pipe", "--run-dir", dir}; !slices.Equal(spec.CaptureFilter, want) {
					t.Errorf("capture filter = %v, want %v", spec.CaptureFilter, want)
				}
				if spec.CaptureAck != filepath.Join(dir, execwrap.CaptureAckFile) {
					t.Errorf("capture ack = %q", spec.CaptureAck)
				}
			} else if spec.CaptureFilter != nil || spec.CaptureAck != "" {
				t.Errorf("a headless Run got a capture filter: %+v", spec)
			}
		})
	}
}

func grantEqual(a, b v1.GrantSecretsRequest) bool {
	return a.LeaseID == b.LeaseID && a.LeaseEpoch == b.LeaseEpoch && a.WorkerEpoch == b.WorkerEpoch && slices.Equal(a.Refs, b.Refs)
}

func TestARunWithoutSecretsIsPreparedAsInPhase1(t *testing.T) {
	for _, mode := range []string{"headless", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			f := secretFixture(t, newFakeStore(nil))
			k := f.acceptedSpec(specJSON("echo hi", "command", mode))
			f.e.step(t.Context(), k)
			if got := f.run(k).Phase; got != state.PhaseRunning {
				t.Fatalf("phase = %s", got)
			}
			if g := f.ctrl.grantRequests(); len(g) != 0 {
				t.Errorf("a Run without secrets asked for a grant: %+v", g)
			}
			if got := f.run(k).PrepareAttempt; got != 0 {
				t.Errorf("prepare attempt = %d, want none taken", got)
			}
			raw, err := os.ReadFile(filepath.Join(f.runDir(k), execwrap.LaunchFile))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("secret")) {
				t.Errorf("launch.json = %s", raw)
			}
			if _, err := os.Stat(filepath.Join(f.runDir(k), "secrets")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a delivery was made: %v", err)
			}
			if s := f.rt.starts[0]; s.CaptureFilter != nil || s.CaptureAck != "" {
				t.Errorf("the session = %+v", s)
			}
		})
	}
}

// deny answers every grant with CF_POLICY_DENIED, each ref for reason.
func deny(reason string) func(string, v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	return func(_ string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		var denied []v1.SecretDenial
		for _, ref := range req.Refs {
			denied = append(denied, v1.SecretDenial{Ref: ref, Reason: reason})
		}
		return v1.GrantSecretsResponse{}, domain.NewError(domain.CodePolicyDenied, "secrets denied", map[string]any{"denied": denied})
	}
}

func TestAGrantDeniedByPolicyFailsThePrepareWithItsCodeAndNoValue(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	f.ctrl.grant = deny(v1.SecretNotAllowlisted)
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	msg := reports[0].Message
	for _, want := range []string{string(domain.CodePolicyDenied), `"github.personal"`, v1.SecretNotAllowlisted} {
		if !strings.Contains(msg, want) {
			t.Errorf("prepare_failed = %q, want %q in it", msg, want)
		}
	}
	if !reports[1].NeverStarted || f.run(k).Phase != state.PhaseStopped {
		t.Errorf("stopped = %+v, phase %s", reports[1], f.run(k).Phase)
	}
	if len(store.reads()) != 0 {
		t.Errorf("a value was read without a grant: %v", store.reads())
	}
	if f.rt.startCount() != 0 {
		t.Error("it started")
	}
	if _, err := os.Stat(filepath.Join(f.runDir(k), "secrets")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a delivery was made: %v", err)
	}
	noValueInReports(t, reports)
}

func TestAStaleGrantFencesTheRunUntilTheReconciliationSaysWhatItIs(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	f.ctrl.grant = func(string, v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		return v1.GrantSecretsResponse{}, domain.NewError(domain.CodeStaleLease, "the lease is not live", map[string]any{"reason": v1.GrantStaleLease})
	}
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	asked := f.e.reconcileGen.Load()
	if f.e.step(t.Context(), k) {
		t.Fatal("a fenced Run ended")
	}
	r := f.run(k)
	if r.Phase != state.PhaseAccepted || r.FencedCode != string(domain.CodeStaleLease) {
		t.Fatalf("phase %s, fenced %q", r.Phase, r.FencedCode)
	}
	if f.e.reconcileGen.Load() == asked {
		t.Error("no reconciliation was asked for")
	}
	if len(f.reports(k)) != 0 || f.rt.startCount() != 0 || len(store.reads()) != 0 {
		t.Errorf("reports %v, starts %d, reads %v", f.reports(k), f.rt.startCount(), store.reads())
	}
	// While fenced, the Run is not prepared again, whatever the time.
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	if n := len(f.ctrl.grantRequests()); n != 1 {
		t.Errorf("a fenced Run asked again: %d grants", n)
	}

	// The reconciliation finds the lease live after all: the Run is unfenced,
	// and its next prepare is a new attempt that asks again.
	f.ctrl.grant = nil
	f.ctrl.reconcile = func(req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
		out := v1.ReconcileResponse{Ack: v1.ReconcileAck{Complete: true}}
		for _, r := range req.Runs {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileContinue, Resend: r.FencedCode != ""})
		}
		return out, nil
	}
	if err := f.e.reconcile(t.Context(), f.e.reconcileGen.Load()); err != nil {
		t.Fatal(err)
	}
	if fenced := f.run(k).FencedCode; fenced != "" {
		t.Fatalf("still fenced: %q", fenced)
	}
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	if r := f.run(k); r.Phase != state.PhaseRunning || r.PrepareAttempt != 2 {
		t.Errorf("phase %s, attempt %d", r.Phase, r.PrepareAttempt)
	}
}

func TestALostGrantAnswerIsAskedAgainAsANewAttemptAfterABackoff(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	calls := 0
	f.ctrl.grant = func(_ string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		calls++
		if calls == 1 {
			// The Controller granted it and the answer was lost on the way.
			return v1.GrantSecretsResponse{}, errors.New("connection reset by peer")
		}
		return v1.GrantSecretsResponse{Granted: req.Refs}, nil
	}
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended")
	}
	if r := f.run(k); r.Phase != state.PhaseAccepted || r.FencedCode != "" || len(f.reports(k)) != 0 {
		t.Fatalf("phase %s, fenced %q, reports %v", r.Phase, r.FencedCode, f.reports(k))
	}
	if len(store.reads()) != 0 {
		t.Errorf("a value was read without a grant: %v", store.reads())
	}
	// Not before the backoff.
	f.e.step(t.Context(), k)
	if calls != 1 {
		t.Fatalf("asked %d times before the backoff", calls)
	}
	f.clock.Advance(prepareRetryInitial)
	f.e.step(t.Context(), k)
	if calls != 2 {
		t.Fatalf("asked %d times after the backoff", calls)
	}
	if r := f.run(k); r.Phase != state.PhaseRunning || r.PrepareAttempt != 2 {
		t.Errorf("phase %s, attempt %d", r.Phase, r.PrepareAttempt)
	}
}

func TestTheCatalogChangingDuringThePrepareFailsIt(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fakeStore)
		code   domain.Code
	}{
		{"the ref was removed", func(s *fakeStore) { _ = s.Delete(context.Background(), "github.personal") }, domain.CodeSecretUnavailable},
		{"the ref was scoped to another Project", func(s *fakeStore) { s.scope("github.personal", "project_9") }, domain.CodePolicyDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(map[string]string{"github.personal": ghValue})
			f := secretFixture(t, store)
			f.ctrl.grant = func(_ string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
				tc.change(store) // between the evaluation and the read
				return v1.GrantSecretsResponse{Granted: req.Refs}, nil
			}
			k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
			if !f.e.step(t.Context(), k) {
				t.Fatal("the Run did not end")
			}
			reports := f.reports(k)
			if len(reports) == 0 || reports[0].Kind != v1.ReportPrepareFailed || !strings.Contains(reports[0].Message, string(tc.code)) {
				t.Errorf("reports = %+v", reports)
			}
			if f.rt.startCount() != 0 {
				t.Error("it started")
			}
			if _, err := os.Stat(filepath.Join(f.runDir(k), "secrets")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a delivery was made: %v", err)
			}
		})
	}
}

func TestSecretsTheOutputCouldNotBeRedactedOfStopTheRunBeforeItStarts(t *testing.T) {
	big := strings.Repeat("v", 60<<10)
	many := map[string]string{}
	env := map[string]string{}
	for i := range 20 {
		ref := "big." + string(rune('a'+i))
		many[ref] = big[:len(big)-i] + string(rune('A'+i))
		env["V_"+string(rune('A'+i))] = ref
	}
	cases := []struct {
		name   string
		values map[string]string
		env    map[string]string
	}{
		{"a value too short to redact", map[string]string{"short": "abc"}, map[string]string{"S": "short"}},
		{"values beyond the budget of redaction", many, env},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := secretFixture(t, newFakeStore(tc.values))
			k := f.acceptedSpec(secretSpec("echo hi", "headless", tc.env))
			if !f.e.step(t.Context(), k) {
				t.Fatal("the Run did not end")
			}
			reports := f.reports(k)
			if len(reports) == 0 || reports[0].Kind != v1.ReportPrepareFailed || !strings.Contains(reports[0].Message, string(domain.CodeSecretUnavailable)) {
				t.Errorf("reports = %+v", reports)
			}
			if f.rt.startCount() != 0 {
				t.Error("it started")
			}
			if _, err := os.Stat(filepath.Join(f.runDir(k), "secrets")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a delivery was made: %v", err)
			}
			raw, _ := json.Marshal(reports)
			for _, v := range tc.values {
				if bytes.Contains(raw, []byte(v)) {
					t.Error("a report holds a value")
				}
			}
		})
	}
}

// adapterSecrets is the command harness with a secret of its own in its Launch.
type adapterSecrets struct {
	harness.Adapter
	env []harness.SecretEnv
}

func (a adapterSecrets) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, host, run)
	l.SecretEnv = a.env
	return l, err
}

func TestARefTheAdapterAsksForAndTheSpecDoesNotDeclareGoesToTheController(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue, "team/api-key": apiValue})
	f := secretFixture(t, store, func(o *Options) {
		o.Harnesses = registry(t, withSecrets{fakeharness.Adapter{Binary: "/fake/harness", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}}})
	})
	f.ctrl.grant = func(_ string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		if slices.Contains(req.Refs, "team/api-key") {
			return deny(v1.SecretNotDeclared)("", req)
		}
		return v1.GrantSecretsResponse{Granted: req.Refs}, nil
	}
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	grants := f.ctrl.grantRequests()
	if len(grants) != 1 || !slices.Equal(grants[0].Refs, []string{"team/api-key"}) {
		t.Errorf("grants = %+v", grants)
	}
	reports := f.reports(k)
	if len(reports) == 0 || !strings.Contains(reports[0].Message, string(domain.CodePolicyDenied)) || !strings.Contains(reports[0].Message, v1.SecretNotDeclared) {
		t.Errorf("reports = %+v", reports)
	}
	if len(store.reads()) != 0 || f.rt.startCount() != 0 {
		t.Errorf("reads %v, starts %d", store.reads(), f.rt.startCount())
	}
}

func TestANameInTheSpecAndInTheAdapterIsRefused(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue, "team/api-key": apiValue})
	f := secretFixture(t, store, func(o *Options) {
		o.Harnesses = registry(t, adapterSecrets{Adapter: command.Adapter{}, env: []harness.SecretEnv{{Name: "GH", Ref: "team/api-key"}}})
	})
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if reports := f.reports(k); len(reports) == 0 || !strings.Contains(reports[0].Message, string(domain.CodeInvalidRequest)) {
		t.Errorf("reports = %+v", reports)
	}
	if len(store.reads()) != 0 || f.rt.startCount() != 0 {
		t.Errorf("reads %v, starts %d", store.reads(), f.rt.startCount())
	}
}

func TestAStopDuringThePrepareCancelsItAndNothingIsDelivered(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	var k state.Key
	f.ctrl.grant = func(_ string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "cancel", SourceController); err != nil {
			t.Error(err)
		}
		return v1.GrantSecretsResponse{Granted: req.Refs}, nil
	}
	k = f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	f.e.step(t.Context(), k)
	f.e.step(t.Context(), k)
	if got := f.run(k).Phase; got != state.PhaseAborted {
		t.Fatalf("phase = %s", got)
	}
	if _, err := os.Stat(filepath.Join(f.runDir(k), "secrets")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a delivery was made after the stop: %v", err)
	}
	if f.rt.startCount() != 0 {
		t.Error("it started")
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
}

func TestTheHarnessLosesItsAccessToTheSecretsOnceItStarted(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	f.e.step(t.Context(), k)
	dir := f.runDir(k)
	h := newWrapperProcess(t, "sleep 30")
	f.writeWrapper(k, h.w)
	f.e.step(t.Context(), k)
	if _, err := secretjit.Resolver(dir)("github.personal"); err != nil {
		t.Fatalf("access was taken before the harness started: %v", err)
	}
	f.writeStarted(k, h.w, t0)
	f.e.step(t.Context(), k)
	if _, err := secretjit.Resolver(dir)("github.personal"); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("the harness still has access: %v", err)
	}
	needles, err := secretjit.Load(dir)
	if err != nil || len(needles) != 1 || string(needles[0].Value) != ghValue {
		t.Errorf("what the output is redacted of went with the access: %v", err)
	}
}
