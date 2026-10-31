//go:build linux || darwin

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/state"
	"github.com/Autarquico/codefabric/internal/worker/workflowfetch"
)

// fakeWorkflows serves one version, as the Controller does for the Run that
// froze it.
type fakeWorkflows struct {
	mu       sync.Mutex
	version  v1.WorkflowVersion
	blobs    map[string]string
	opened   map[string]int
	manifest int
	err      error
	// tamper replaces the content served for a digest.
	tamper map[string]string
}

func newFakeWorkflows(t *testing.T, files map[string]string) *fakeWorkflows {
	t.Helper()
	w := &fakeWorkflows{blobs: map[string]string{}, opened: map[string]int{}, tamper: map[string]string{}}
	comp := domain.ManifestComponent{
		Harness: fakeharness.DefaultID, Name: fakeharness.SettingsComponent, Kind: string(harness.KindJSONSubset), Ownership: domain.OwnershipManaged,
	}
	var hf []harness.File
	for path, content := range files {
		id := workflowfetch.SHA256Of([]byte(content))
		w.blobs[id] = content
		comp.Files = append(comp.Files, domain.ManifestFile{Path: path, SHA256: id, SecretRefs: []string{}})
		hf = append(hf, harness.File{Path: path, Mode: 0o600, Content: []byte(content)})
	}
	tree, err := harness.FilesDigest(hf)
	if err != nil {
		t.Fatal(err)
	}
	comp.TreeDigest = tree
	w.version = v1.WorkflowVersion{
		Workflow: "team", Version: 1,
		Manifest: &domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Components: []domain.ManifestComponent{comp}},
	}
	w.version.Digest, err = w.version.Manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *fakeWorkflows) RunWorkflow(context.Context, string, uint64) (v1.WorkflowVersion, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.manifest++
	return w.version, w.err
}

func (w *fakeWorkflows) OpenRunBlob(_ context.Context, _ string, _ uint64, digest string) (io.ReadCloser, int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opened[digest]++
	if w.err != nil {
		return nil, 0, w.err
	}
	content, ok := w.tamper[digest]
	if !ok {
		content = w.blobs[digest]
	}
	return io.NopCloser(strings.NewReader(content)), int64(len(content)), nil
}

func (w *fakeWorkflows) opens() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.opened {
		n += c
	}
	return n
}

// spec is the spec of a Run that froze the version this fake serves.
func (w *fakeWorkflows) spec(prompt string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"schema": 2, "prompt": prompt, "mode": "headless", "workflow": "team@" + w.version.Digest,
		"requirements": map[string]any{"harness": fakeharness.DefaultID},
	})
	return raw
}

func workflowFixture(t *testing.T, w *fakeWorkflows) *fixture {
	t.Helper()
	return newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, isolated)
		o.Workflows = w
	})
}

func startedDigest(f *fixture, k state.Key) (string, bool) {
	for _, e := range f.reports(k) {
		if e.Kind == v1.ReportStarted {
			return e.ConfigDigest, true
		}
	}
	return "", false
}

func TestARunWithAWorkflowIsMaterialisedFromItsFrozenVersionAndReportsTheDigest(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{"theme":"dark"}`})
	f := workflowFixture(t, w)
	k := f.acceptedSpec(w.spec("do it"))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	got, err := os.ReadFile(filepath.Join(f.runDir(k), "config", "settings.json")) //nolint:gosec // a file of the test
	if err != nil || string(got) != `{"theme":"dark"}` {
		t.Fatalf("config/settings.json = %q (%v)", got, err)
	}
	mat, err := host.ReadMaterialization(f.runDir(k))
	if err != nil || mat.Digest == "" {
		t.Fatalf("materialization = %+v (%v)", mat, err)
	}
	if kept, ok, err := state.ConfigDigestOf(t.Context(), f.db.Reader(), k); err != nil || !ok || kept != mat.Digest {
		t.Errorf("the Worker kept %q (%v, %v), want %s", kept, ok, err, mat.Digest)
	}
	if digest, ok := startedDigest(f, k); !ok || digest != mat.Digest {
		t.Errorf("started carries %q (reported=%v), want the digest of the materialisation %s", digest, ok, mat.Digest)
	}
	// The blob was downloaded once and kept: the assignment check and the prepare share it.
	if n := w.opens(); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
	cached := filepath.Join(f.paths.WorkerDataDir(), workflowfetch.CacheDir, strings.TrimPrefix(workflowfetch.SHA256Of([]byte(`{"theme":"dark"}`)), "sha256:"))
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("the blob is not in the cache: %v", err)
	}
}

func TestARunWithoutAWorkflowReportsNoConfigDigest(t *testing.T) {
	f := workflowFixture(t, newFakeWorkflows(t, map[string]string{"settings.json": `{}`}))
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	if digest, ok := startedDigest(f, k); !ok || digest != "" {
		t.Errorf("started carries %q (reported=%v), want none", digest, ok)
	}
}

func TestACachedBlobIsNotDownloadedAgainButIsCheckedAgain(t *testing.T) {
	const content = `{"theme":"dark"}`
	w := newFakeWorkflows(t, map[string]string{"settings.json": content})
	f := workflowFixture(t, w)
	run := state.Run{Key: state.Key{RunID: runID(1), LeaseEpoch: 1}, Spec: w.spec("x")}
	if _, err := f.e.config(t.Context(), run, fakeharness.DefaultID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.config(t.Context(), run, fakeharness.DefaultID); err != nil {
		t.Fatal(err)
	}
	if w.opens() != 1 || w.manifest != 2 {
		t.Fatalf("downloads = %d, manifests = %d, want 1 and 2", w.opens(), w.manifest)
	}
	// What the cache holds is not trusted: a file that no longer hashes is fetched again.
	cached := filepath.Join(f.paths.WorkerDataDir(), workflowfetch.CacheDir, strings.TrimPrefix(workflowfetch.SHA256Of([]byte(content)), "sha256:"))
	if err := os.WriteFile(cached, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := f.e.config(t.Context(), run, fakeharness.DefaultID)
	if err != nil || string(snap.Components[0].Files[0].Content) != content || w.opens() != 2 {
		t.Errorf("after tampering: %v, %d downloads, snapshot %+v", err, w.opens(), snap)
	}
}

func TestATamperedBlobIsRejectedAndNeverKept(t *testing.T) {
	const content = `{"theme":"dark"}`
	w := newFakeWorkflows(t, map[string]string{"settings.json": content})
	w.tamper[workflowfetch.SHA256Of([]byte(content))] = `{"theme":"evil"}`
	f := workflowFixture(t, w)
	k := f.acceptedSpec(w.spec("do it"))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if len(reports) == 0 || reports[0].Kind != v1.ReportPrepareFailed || !strings.Contains(reports[0].Message, "does not hash") {
		t.Errorf("reports = %+v", reports)
	}
	if _, err := os.Stat(filepath.Join(f.runDir(k), "config")); err == nil {
		t.Error("a configuration was materialised from a tampered blob")
	}
	entries, _ := os.ReadDir(filepath.Join(f.paths.WorkerDataDir(), workflowfetch.CacheDir))
	if len(entries) != 0 {
		t.Errorf("the cache holds %d entries after a rejected download", len(entries))
	}
}

func TestAManifestThatIsNotTheFrozenOneOrDoesNotDigestIsRejected(t *testing.T) {
	cases := map[string]func(*fakeWorkflows){
		"another version": func(w *fakeWorkflows) { w.version.Digest = "sha256:" + strings.Repeat("2", 64) },
		"the label of the version on another manifest": func(w *fakeWorkflows) { w.version.Manifest.Components[0].Name = "other" },
		"a tree digest that is not that of the files": func(w *fakeWorkflows) {
			w.version.Manifest.Components[0].TreeDigest = "sha256:" + strings.Repeat("3", 64)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
			mutate(w)
			f := workflowFixture(t, w)
			run := state.Run{Key: state.Key{RunID: runID(1), LeaseEpoch: 1}, Spec: w.spec("x")}
			if _, err := f.e.config(t.Context(), run, fakeharness.DefaultID); err == nil || errors.Is(err, errPrepareLater) {
				t.Errorf("err = %v, want a final refusal", err)
			}
		})
	}
}

func TestADownloadThatCannotBeAnsweredNowIsTriedAgainAndARefusalIsFinal(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	f := workflowFixture(t, w)
	run := state.Run{Key: state.Key{RunID: runID(1), LeaseEpoch: 1}, Spec: w.spec("x")}
	for code, later := range map[domain.Code]bool{
		domain.CodeControllerUnreachable: true, domain.CodeStaleLease: true, domain.CodePolicyDenied: false, domain.CodeNotFound: false,
	} {
		w.err = domain.NewError(code, "no", nil)
		_, err := f.e.config(t.Context(), run, fakeharness.DefaultID)
		if err == nil || errors.Is(err, errPrepareLater) != later {
			t.Errorf("%s: err = %v, later = %v", code, err, later)
		}
	}
}

func TestAWorkflowRunOfAWorkerWithoutASourceDoesNotRunWithAnEmptyConfiguration(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	f := newFixture(t, func(o *Options) { o.Harnesses = registry(t, isolated) })
	run := state.Run{Key: state.Key{RunID: runID(1), LeaseEpoch: 1}, Spec: w.spec("x")}
	if _, err := f.e.config(t.Context(), run, fakeharness.DefaultID); err == nil {
		t.Error("a Run that froze a Workflow got a configuration from nowhere")
	}
}

// Until a harness without ConfigIsolation can be given the Workflow, a Run that
// froze one is turned away, not started with the configuration of the host.
func TestAWorkflowRunOfAHarnessWithoutConfigIsolationIsRefused(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	f := newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, command.Adapter{})
		o.Workflows = w
	})
	raw, _ := json.Marshal(map[string]any{
		"schema": 2, "prompt": "x", "mode": "headless", "workflow": "team@" + w.version.Digest,
		"requirements": map[string]any{"harness": command.ID},
	})
	run := state.Run{Key: state.Key{RunID: runID(1), LeaseEpoch: 1}, Spec: raw, CPU: 1, MemoryMB: 1}
	v := f.e.evaluate(t.Context(), run, Limits{MaxConcurrentRuns: 4, CPU: 8, MemoryMB: 1024}, state.Load{})
	if v.reject != domain.CodeHarnessUnavailable || w.manifest != 0 {
		t.Errorf("verdict = %+v, manifests read = %d", v, w.manifest)
	}
}
