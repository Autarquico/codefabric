//go:build integration && (linux || darwin)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client/workflowadopt"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// The secrets of the fixture are assembled at run time, so that no scanner of
// the repository mistakes them for real ones.
var (
	authSecret  = "auth-" + "never-adopted-91c2"
	githubToken = "ghp_" + strings.Repeat("aB3dE5fG7h", 3) + "aB3dE5"
)

const codexConfig = `model = "gpt-5"

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs-mcp"]
`

// codexHome is a temporary HOME with a ~/.codex the way a person has one, and
// an auth.json next to it. The ~/.claude half of the Given of the Adopt scenario
// is closed by #20 (P1): until the Claude adapter exists there is no snapshot
// of it to take.
func codexHome(t *testing.T, config string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	for path, content := range map[string]string{
		"config.toml":            config,
		"AGENTS.md":              "Always answer in Spanish.\n",
		"skills/review/SKILL.md": "---\nname: review\ndescription: review a diff\n---\nRead the diff.\n",
		"auth.json":              `{"tokens":{"access_token":"` + authSecret + `"}}`,
	} {
		writeCodex(t, dir, path, content)
	}
	return home
}

func writeCodex(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// storedBytes is everything the Controller keeps under its data directory:
// blobs, database, logs.
func storedBytes(t *testing.T, host config.Paths) string {
	t.Helper()
	var all strings.Builder
	err := filepath.WalkDir(host.DataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the test's own data directory
		all.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return all.String()
}

func blobCount(t *testing.T, host config.Paths) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(host.DataDir, "blobs"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func adoptArgs(name string, extra ...string) []string {
	return append([]string{"workflow", "adopt", name, "--harness", "codex", "--yes"}, extra...)
}

func commitArgs(name string, extra ...string) []string {
	return append([]string{"workflow", "commit", name, "--yes"}, extra...)
}

func mustContainComponents(t *testing.T, m *domain.Manifest, kinds ...string) {
	t.Helper()
	if m == nil || len(m.Components) == 0 {
		t.Fatalf("manifest = %+v", m)
	}
	have := map[string]bool{}
	for _, c := range m.Components {
		if c.Harness != "codex" || !strings.HasPrefix(c.TreeDigest, "sha256:") || len(c.Files) == 0 {
			t.Errorf("component %+v", c)
		}
		have[c.Kind] = true
	}
	for _, k := range kinds {
		if !have[k] {
			t.Errorf("no %q component in %+v", k, m.Components)
		}
	}
}

// Adopt: settings, skills and MCP of ~/.codex become v1 with components and
// digests, and auth.json is in no blob and in no output.
func TestScenario_WorkflowStoreAdoptCommit_Adopt_E2E(t *testing.T) {
	host, _ := bringUpController(t)
	codexHome(t, codexConfig)

	v := ok[v1.WorkflowVersion](t, adoptArgs("daily")...)
	if v.Workflow != "daily" || v.Version != 1 || !strings.HasPrefix(v.Digest, "sha256:") {
		t.Fatalf("adopted = %+v", v)
	}

	shown := ok[v1.WorkflowVersion](t, "workflow", "show", "daily")
	if shown.Digest != v.Digest || shown.Version != 1 {
		t.Errorf("show = v%d %s, adopt gave %s", shown.Version, shown.Digest, v.Digest)
	}
	mustContainComponents(t, shown.Manifest, "file", "dir")

	_, human, errOut := cf(t, "", "workflow", "show", "daily")
	_, listed, _ := cf(t, "", "workflow", "list", "--json")
	_, history, _ := cf(t, "", "workflow", "history", "daily", "--json")
	if blobCount(t, host) == 0 {
		t.Error("the Controller holds no blob of the adopted files")
	}
	seen := map[string]string{
		"adopt --json": mustJSON(t, v), "show --json": mustJSON(t, shown), "show": human + errOut,
		"list": listed, "history": history, "the data directory": storedBytes(t, host),
	}
	for where, text := range seen {
		if strings.Contains(text, authSecret) || strings.Contains(text, "access_token") {
			t.Errorf("auth.json content is in %s", where)
		}
	}
	if !strings.Contains(seen["the data directory"], "Always answer in Spanish.") {
		t.Error("the AGENTS.md content should be stored: the test would prove nothing otherwise")
	}
}

// Secretos en settings: a token in the config aborts a headless adopt before
// anything leaves the machine; with --replace-secrets the value is nowhere and
// the ref is in the manifest.
func TestScenario_WorkflowStoreAdoptCommit_SecretosEnSettings_E2E(t *testing.T) {
	host, _ := bringUpController(t)
	codexHome(t, codexConfig+"\n[shell_environment_policy.set]\nGITHUB_TOKEN = \""+githubToken+"\"\n")

	code, out, errOut := cf(t, "", adoptArgs("daily")...)
	if code != 2 || !strings.Contains(errOut, string(domain.CodeInvalidRequest)) {
		t.Fatalf("without --replace-secrets: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if strings.Contains(out+errOut, githubToken) {
		t.Error("the refusal prints the token")
	}
	if blobCount(t, host) != 0 {
		t.Errorf("%d blobs uploaded by an adopt that aborted", blobCount(t, host))
	}
	if list := ok[v1.ListWorkflowsResponse](t, "workflow", "list"); len(list.Workflows) != 0 {
		t.Errorf("the aborted adopt left %+v", list.Workflows)
	}
	if strings.Contains(storedBytes(t, host), githubToken) {
		t.Error("the token reached the Controller despite the abort")
	}

	code, out, errOut = cf(t, "", append(adoptArgs("daily", "--replace-secrets"), "--json")...)
	if code != 0 {
		t.Fatalf("with --replace-secrets: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	v := ok[v1.WorkflowVersion](t, "workflow", "show", "daily")
	manifest := mustJSON(t, v.Manifest)
	if !strings.Contains(manifest, `"secret_refs":["codex/shell_environment_policy/set/GITHUB_TOKEN"]`) {
		t.Errorf("the manifest should carry the secret ref: %s", manifest)
	}
	for where, text := range map[string]string{"adopt output": out + errOut, "manifest": manifest, "the data directory": storedBytes(t, host)} {
		if strings.Contains(text, githubToken) {
			t.Errorf("the token is in %s", where)
		}
	}
}

// racingAdopter lets a rival commit its version right before this commit
// reaches the Controller: both were made on the same parent.
type racingAdopter struct {
	cli.WorkflowAdopter
	rival func()
	once  sync.Once
}

func (r *racingAdopter) Commit(ctx context.Context, name string, p *workflowadopt.Prepared, parent int, changelog string) (v1.WorkflowVersion, error) {
	r.once.Do(r.rival)
	return r.WorkflowAdopter.Commit(ctx, name, p, parent, changelog)
}

// Conflicto: of two commits on the same parent one wins and the other exits 4
// with CF_WORKFLOW_CONFLICT.
func TestScenario_WorkflowStoreAdoptCommit_Conflicto_E2E(t *testing.T) {
	bringUpController(t)
	home := codexHome(t, codexConfig)
	ok[v1.WorkflowVersion](t, adoptArgs("daily")...)
	writeCodex(t, filepath.Join(home, ".codex"), "AGENTS.md", "Always answer in French.\n")

	opts := options()
	inner := opts.Workflows
	opts.Workflows = func(s workflowadopt.Store, c workflowadopt.Confirmer) cli.WorkflowAdopter {
		return &racingAdopter{WorkflowAdopter: inner(s, c), rival: func() {
			if v := ok[v1.WorkflowVersion](t, commitArgs("daily")...); v.Version != 2 {
				t.Errorf("the rival got v%d", v.Version)
			}
		}}
	}
	var out, errOut bytes.Buffer
	code := cli.RunContextWith(t.Context(), opts, commitArgs("daily"), strings.NewReader(""), &out, &errOut, daemon.Command(), workerdaemon.Command())
	if code != 4 || !strings.Contains(errOut.String(), string(domain.CodeWorkflowConflict)) {
		t.Fatalf("the loser: exit %d\nstdout: %s\nstderr: %s", code, &out, &errOut)
	}
	history := ok[v1.WorkflowHistoryResponse](t, "workflow", "history", "daily")
	if len(history.Versions) != 2 || history.Versions[0].Version != 2 {
		t.Errorf("history = %+v, want exactly the winner on top of v1", history.Versions)
	}
}

// Conflicto, without a staged race: two `cf workflow commit` at once never
// produce two children of the same parent.
func TestWorkflowStoreAdoptCommit_ConcurrentCommitsMakeOneChild(t *testing.T) {
	bringUpController(t)
	home := codexHome(t, codexConfig)
	ok[v1.WorkflowVersion](t, adoptArgs("daily")...)
	writeCodex(t, filepath.Join(home, ".codex"), "AGENTS.md", "Always answer in French.\n")

	codes := make([]int, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], _, _ = cf(t, "", commitArgs("daily")...)
		}()
	}
	close(start)
	wg.Wait()

	for _, c := range codes {
		if c != 0 && c != 4 {
			t.Errorf("exit codes %v: a loser exits 4, one that arrives late finds nothing to commit", codes)
		}
	}
	history := ok[v1.WorkflowHistoryResponse](t, "workflow", "history", "daily")
	if len(history.Versions) != 2 {
		t.Errorf("%d versions, want v1 and one child", len(history.Versions))
	}
}

// Congelación: a Run created with name@latest keeps its version and digest
// when v2 is committed, and with no Worker able to materialise the config
// (its harness has no config isolation) it stays queued.
func TestScenario_WorkflowStoreAdoptCommit_Congelacion_E2E(t *testing.T) {
	host, _ := bringUpController(t)
	home := codexHome(t, codexConfig)
	v1c := ok[v1.WorkflowVersion](t, adoptArgs("daily")...)

	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: box, Interval: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), Runtime: &idleRuntime{},
			Harnesses: []harness.Adapter{fakeharness.Adapter{Binary: self, Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	soon(t, "the Worker to be online", func() bool { return showWorker(t, host, "alpha").State == domain.WorkerOnline })
	isolation := "harness:" + fakeharness.DefaultID + ":config-isolation"
	if caps := strings.Join(showWorker(t, host, "alpha").Capabilities, " "); strings.Contains(caps, isolation) {
		t.Fatalf("the Worker offers %s: the premise of the test is gone", isolation)
	}

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend", "--workflow", "daily@latest")
	created := ok[v1.CreateTaskResponse](t, "do", "x", "--harness", fakeharness.DefaultID)
	run := created.Run
	if run == nil || run.Workflow == nil || run.Workflow.Version != 1 || run.Workflow.Digest != v1c.Digest {
		t.Fatalf("created Run = %+v", run)
	}

	writeCodex(t, filepath.Join(home, ".codex"), "AGENTS.md", "Always answer in French.\n")
	v2 := ok[v1.WorkflowVersion](t, commitArgs("daily")...)
	if v2.Version != 2 || v2.Digest == v1c.Digest {
		t.Fatalf("v2 = %+v", v2)
	}

	shown := ok[v1.Run](t, "run", "show", run.Short)
	if shown.Workflow == nil || shown.Workflow.Version != 1 || shown.Workflow.Digest != v1c.Digest {
		t.Errorf("the Run moved to %+v, want v1 %s", shown.Workflow, v1c.Digest)
	}
	if !strings.Contains(string(shown.Spec), `"workflow":"daily@`+v1c.Digest+`"`) {
		t.Errorf("frozen spec = %s", shown.Spec)
	}

	// P2: queued, never handed to a Worker that cannot materialise the config.
	explained := ok[v1.RunExplanation](t, "run", "explain", run.Short)
	if explained.Decision != v1.DecisionNoWorker || explained.Lease != nil || explained.Chosen != "" {
		t.Errorf("explanation = %+v", explained)
	}
	if len(explained.Workers) != 1 || len(explained.Workers[0].Failed) != 1 || explained.Workers[0].Failed[0].Filter != "capabilities" {
		t.Errorf("the Worker should fail only on %s: %+v", isolation, explained.Workers)
	}
	// The scheduler has looked at the Run and left it queued with that reason:
	// waiting on the fact, not on the clock.
	soon(t, "the scheduler to have refused the Run", func() bool {
		return ok[v1.Run](t, "run", "show", run.Short).QueuedReason == string(domain.CodeNoEligibleWorker)
	})
	if later := ok[v1.Run](t, "run", "show", run.Short); later.State != domain.RunQueued || later.Lease != nil || later.WorkerID != "" {
		t.Errorf("Run = %+v, want queued with no Worker", later)
	}
	if task := ok[v1.CreateTaskResponse](t, "do", "y", "--harness", fakeharness.DefaultID); task.Run.Workflow == nil || task.Run.Workflow.Version != 2 {
		t.Errorf("a new Run should meet the new head: %+v", task.Run.Workflow)
	}
}

// Digest estable: the same content is the same digest, and committing it again
// is refused as nothing to commit. Order independence is
// internal/domain TestScenario_DigestEstable.
func TestScenario_WorkflowStoreAdoptCommit_DigestEstable_E2E(t *testing.T) {
	bringUpController(t)
	codexHome(t, codexConfig)

	first := ok[v1.WorkflowVersion](t, adoptArgs("daily")...)
	again := ok[v1.WorkflowVersion](t, commitArgs("daily")...)
	if again.Version != 1 || again.Digest != first.Digest {
		t.Errorf("a commit of the same content gave v%d %s, want v1 %s", again.Version, again.Digest, first.Digest)
	}
	_, _, errOut := cf(t, "", commitArgs("daily")...)
	if !strings.Contains(errOut, "No changes") {
		t.Errorf("stderr = %q, want it to say there is nothing to commit", errOut)
	}
	if h := ok[v1.WorkflowHistoryResponse](t, "workflow", "history", "daily"); len(h.Versions) != 1 {
		t.Errorf("history = %+v, want only v1", h.Versions)
	}

	// Another Workflow adopting the same content has the same digest.
	other := ok[v1.WorkflowVersion](t, adoptArgs("twin")...)
	if other.Digest != first.Digest {
		t.Errorf("the same content digests to %s and %s", first.Digest, other.Digest)
	}
}
