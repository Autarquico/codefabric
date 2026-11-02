//go:build e2e && (linux || darwin)

package e2e

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/host"
)

// The scenarios of the spec e2e-v0-2 (§42.4 #6, #7 and #12 and the Done of
// Fase 2): a Workflow adopted on the machine of Worker A travels, through the
// Controller, to the HOME of Worker B and to its Runs.
//
// Not covered yet, by want of the features: §42.4 #12 also asks that no secret
// reach a snapshot or a backup of the Controller. Neither exists in this phase;
// the check of the data directory below is what stands for them until then.

// The secrets of the fixture are assembled at run time, so that no scanner of
// the repository mistakes them for real ones. A's are what its configuration
// holds; B's are what B's secret store holds for the same refs.
var (
	codexSecretA  = "sk-" + "e2eA" + strings.Repeat("c0dexA", 4)
	claudeSecretA = "sk-ant-" + "e2eA" + strings.Repeat("c1audeA", 4)
	codexSecretB  = "sk-" + "e2eB" + strings.Repeat("c0dexB", 4)
	claudeSecretB = "sk-ant-" + "e2eB" + strings.Repeat("c1audeB", 4)
	codexAuthA    = "codex-auth-" + "e2eA-never-adopted"
	claudeAuthA   = "claude-auth-" + "e2eA-never-adopted"
)

const (
	codexSecretRef  = "codex/mcp/docs/env/DOCS_API_KEY" //nolint:gosec // a secret ref, not a secret
	claudeSecretRef = "claude/settings/env/ACME_API_KEY"
	workflowName    = "team"
	spanish         = "Always answer in Spanish.\n"
	french          = "Always answer in French.\n"
	german          = "Always answer in German.\n"
)

func codexConfig(secret string) string {
	return `model = "gpt-5"

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs-mcp"]

[mcp_servers.docs.env]
DOCS_API_KEY = "` + secret + `"
`
}

func claudeSettings(secret string) string {
	return `{
  "env": {
    "ACME_API_KEY": "` + secret + `"
  },
  "model": "opus"
}
`
}

// phase2 is a cluster whose Worker A (w1) holds a person's configuration of
// Codex and Claude Code and whose Worker B (w2) has both harnesses installed
// and logged in, and holds its own values of the secrets of that
// configuration. A has no harness in its PATH: every Codex Run goes to B. A
// client is connected in A's box, as the person at that machine.
type phase2 struct {
	*Cluster
	a, b *Worker
}

func newPhase2(t *testing.T, o Opts) *phase2 {
	t.Helper()
	o.Workers = 2
	o.Prepare = func(name string, b *Box) {
		switch name {
		case "w1":
			b.WriteHome(".codex/config.toml", codexConfig(codexSecretA))
			b.WriteHome(".codex/AGENTS.md", spanish)
			b.WriteHome(".codex/skills/review/SKILL.md", "---\nname: review\ndescription: review a diff\n---\nRead the diff.\n")
			b.WriteHome(".codex/auth.json", `{"tokens":{"access_token":"`+codexAuthA+`"}}`)
			b.WriteHome(".claude/settings.json", claudeSettings(claudeSecretA))
			b.WriteHome(".claude/CLAUDE.md", "Prefer small diffs.\n")
			b.WriteHome(".claude/agents/reviewer.md", "---\nname: reviewer\n---\nReview.\n")
			b.WriteHome(".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"`+claudeAuthA+`"}}`)
		case "w2":
			b.InstallHarnesses()
		}
	}
	p := &phase2{Cluster: NewCluster(t, o)}
	p.a, p.b = p.Workers[0], p.Workers[1]
	p.Connect(p.a.Box, "alice")
	for ref, value := range map[string]string{codexSecretRef: codexSecretB, claudeSecretRef: claudeSecretB} {
		if res := p.CFInput(p.b.Box, value, "worker", "secrets", "set", ref); res.Code != 0 {
			t.Fatalf("e2e: %s", res)
		}
	}
	return p
}

// adopt is `cf workflow adopt` of both harnesses at A, the secrets replaced by markers.
func (p *phase2) adopt() v1.WorkflowVersion {
	p.t.Helper()
	return JSON[v1.WorkflowVersion](p.Cluster, p.a.Box, "workflow", "adopt", workflowName,
		"--harness", "codex", "--harness", "claude-code", "--replace-secrets", "--yes")
}

// commitAgents commits, at A, a version whose ~/.codex/AGENTS.md says text.
func (p *phase2) commitAgents(text string) v1.WorkflowVersion {
	p.t.Helper()
	p.a.Box.WriteHome(".codex/AGENTS.md", text)
	return JSON[v1.WorkflowVersion](p.Cluster, p.a.Box, "workflow", "commit", workflowName, "--replace-secrets", "--yes")
}

// applyToB is `cf workflow apply team@latest` of harness to B, by the owner.
func (p *phase2) applyToB(harnessID string, flags ...string) v1.ApplyWorkflowResponse {
	p.t.Helper()
	args := append([]string{"workflow", "apply", workflowName + "@latest", "--worker", p.b.Name, "--harness", harnessID}, flags...)
	return JSON[v1.ApplyWorkflowResponse](p.Cluster, p.Ctl, args...)
}

func (p *phase2) stateOfB(harnessID string) v1.WorkflowState {
	p.t.Helper()
	states := JSON[v1.WorkflowStateResponse](p.Cluster, p.Ctl, "workflow", "status", "--worker", p.b.Name, "--harness", harnessID).States
	if len(states) != 1 {
		p.t.Fatalf("e2e: workflow status = %+v, want one state", states)
	}
	return states[0]
}

func driftOf(s v1.WorkflowState) string {
	names := make([]string, len(s.Drift))
	for i, d := range s.Drift {
		names[i] = d.Component
	}
	return strings.Join(names, ",")
}

// persisted is every file a secret must never reach, by path: the data and
// configuration of the Controller (its database, its blobs, its logs), the log
// of every process of the cluster (the daemons, the Controller, and the
// stdout and stderr of every cf command, the CLI output) and what B keeps of
// its Runs but their configuration directory, which holds the rendered
// secrets while the Run lives.
func (p *phase2) persisted() map[string]string {
	p.t.Helper()
	files := map[string]string{}
	read := func(root string, skip func(path string, d fs.DirEntry) bool) {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if skip != nil && skip(path, d) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			raw, err := os.ReadFile(path) //nolint:gosec // a file of the test's own cluster
			files[path] = string(raw)
			return err
		})
		if err != nil {
			p.t.Fatal(err)
		}
	}
	read(p.Ctl.Paths.DataDir, nil)
	read(p.Ctl.Paths.ConfigDir, nil)
	p.H.mu.Lock()
	boxes := slices.Clone(p.H.boxes)
	p.H.mu.Unlock()
	for _, b := range boxes {
		read(b.dirs.logs, nil)
	}
	// The whole data of each Worker (its state.db and WAL, journals, run
	// directories), but the configuration rendered for a Run, which holds the
	// secret by design.
	for _, w := range []*Worker{p.a, p.b} {
		data := w.Box.Paths.WorkerDataDir()
		if _, err := os.Stat(data); err != nil {
			continue
		}
		read(data, func(path string, d fs.DirEntry) bool {
			runDir := filepath.Dir(path)
			return d.IsDir() && filepath.Base(filepath.Dir(runDir)) == "runs" &&
				(path == host.ConfigDir(runDir) || path == host.StagingDir(runDir))
		})
	}
	return files
}

// noSecretIn fails for every secret value found in a persisted file.
func (p *phase2) noSecretIn(files map[string]string, values ...string) {
	p.t.Helper()
	for path, content := range files {
		for _, v := range values {
			if strings.Contains(content, v) {
				p.t.Errorf("a secret value (%.8s...) is in %s", v, path)
			}
		}
	}
}

// TestScenario_E2eV02_DoneDeFase2 (#6, #12): the configuration of Claude and
// Codex adopted at A and applied selectively to B is reproduced in B, the
// chosen components and no others, with the secrets B holds; and no secret
// value, of A or of B, is in a blob, a log or an output, not even the one a Run
// on B prints.
func TestScenario_E2eV02_DoneDeFase2(t *testing.T) {
	p := newPhase2(t, Opts{})
	adopted := p.adopt()
	if adopted.Version != 1 || !strings.HasPrefix(adopted.Digest, "sha256:") {
		t.Fatalf("adopted = %+v", adopted)
	}
	shown := JSON[v1.WorkflowVersion](p.Cluster, p.a.Box, "workflow", "show", workflowName)
	manifest, err := json.Marshal(shown.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{codexSecretRef, claudeSecretRef} {
		if !strings.Contains(string(manifest), `"`+ref+`"`) {
			t.Errorf("the manifest does not carry the secret ref %s: %s", ref, manifest)
		}
	}

	// Selective: of each harness, two components; the skills of Codex and the
	// agents of Claude stay at A.
	for harnessID, components := range map[string]string{"codex": "config.toml,AGENTS.md", "claude-code": "settings.json,CLAUDE.md"} {
		res := p.applyToB(harnessID, "--components", components)
		if res.Apply == nil || res.Apply.State != v1.ApplyStateApplied {
			t.Fatalf("apply of %s to B = %+v", harnessID, res)
		}
	}
	b := p.b.Box
	for rel, want := range map[string]string{
		".codex/AGENTS.md":          spanish,
		".claude/CLAUDE.md":         "Prefer small diffs.\n",
		".codex/auth.json":          "{}",
		".claude/.credentials.json": "{}",
	} {
		if got := b.ReadHome(rel); got != want {
			t.Errorf("B's %s = %q, want %q", rel, got, want)
		}
	}
	if got, want := parseTOML(t, b.ReadHome(".codex/config.toml")), parseTOML(t, codexConfig(codexSecretB)); !reflect.DeepEqual(got, want) {
		t.Errorf("B's config.toml = %v, want A's with B's secret: %v", got, want)
	}
	var settings struct {
		Env   map[string]string `json:"env"`
		Model string            `json:"model"`
	}
	if err := json.Unmarshal([]byte(b.ReadHome(".claude/settings.json")), &settings); err != nil {
		t.Errorf("B's settings.json: %v", err)
	}
	if settings.Env["ACME_API_KEY"] != claudeSecretB || settings.Model != "opus" {
		t.Errorf("B's settings.json = %+v, want A's with B's secret", settings)
	}
	for _, rel := range []string{".codex/skills", ".claude/agents"} {
		if _, err := os.Stat(filepath.Join(b.Home(), rel)); !os.IsNotExist(err) {
			t.Errorf("B has %s, which no apply chose (%v)", rel, err)
		}
	}

	// A Run on B gets B's codex secret and prints it: what is kept of it says
	// the ref, never the value.
	p.mustCF(p.Ctl, "project", "secrets", "allow", "e2e", codexSecretRef)
	created := p.Do(`echo "token=$TOKEN"`, "--secret-env", "TOKEN="+codexSecretRef)
	run := p.WaitRun(created.Run.ID, "settled", func(r v1.Run) bool { return settled(r.State) && r.Runtime != domain.RuntimeActive })
	if run.State != domain.RunCompleted || run.WorkerID != p.b.ID {
		t.Fatalf("the Run that uses B's secret: %s", RunStage(run))
	}
	p.Eventually("the output of the Run to reach the Controller", func() (bool, string) {
		out := p.mustCF(p.Observer, "logs", "--run", run.ID).Stdout
		return strings.Contains(out, "token=[REDACTED:"+codexSecretRef+"]"), out
	})
	// A Codex Run on B gets the Workflow with B's secret rendered into its
	// configuration, and prints that configuration: the same.
	p.mustCF(p.Ctl, "project", "set", "e2e", "--workflow", workflowName+"@latest")
	withWorkflow := JSON[v1.CreateTaskResponse](p.Cluster, p.Ctl, "do", "say ok", "--harness", "codex")
	codexRun := p.WaitRun(withWorkflow.Run.ID, "settled", func(r v1.Run) bool { return settled(r.State) && r.Runtime != domain.RuntimeActive })
	if codexRun.State != domain.RunCompleted || codexRun.WorkerID != p.b.ID {
		t.Fatalf("the Codex Run: %s", RunStage(codexRun))
	}
	p.Eventually("the output of the Codex Run to reach the Controller", func() (bool, string) {
		out := p.mustCF(p.Observer, "logs", "--run", codexRun.ID).Stdout
		return strings.Contains(out, "DOCS_API_KEY") && strings.Contains(out, "[REDACTED:"+codexSecretRef+"]"), out
	})

	p.mustCF(p.a.Box, "workflow", "show", workflowName)
	p.Eventually("B to release the secrets of the Run", func() (bool, string) {
		left, _ := filepath.Glob(filepath.Join(p.b.Box.Paths.WorkerDataDir(), "runs", "*", "secrets*"))
		return len(left) == 0, strings.Join(left, " ")
	})
	p.mustCF(p.Ctl, "events", "--run", run.ID, "--json")
	p.mustCF(p.Ctl, "run", "show", codexRun.ID)

	p.noSecretIn(p.persisted(), codexSecretA, claudeSecretA, codexSecretB, claudeSecretB, codexAuthA, claudeAuthA)
	if !strings.Contains(strings.Join(slices.Collect(maps.Values(p.persisted())), ""), spanish) {
		t.Error("the content of AGENTS.md is in no persisted file: the search for secrets would prove nothing")
	}
}

// TestScenario_E2eV02_Drift (#7): a managed component edited on B is not
// overwritten by an apply without --force, which leaves the state partial with
// the drift said; with --force it is.
func TestScenario_E2eV02_Drift(t *testing.T) {
	p := newPhase2(t, Opts{})
	p.adopt()
	p.applyToB("codex")
	if got := p.b.Box.ReadHome(".codex/AGENTS.md"); got != spanish {
		t.Fatalf("the first apply left AGENTS.md = %q", got)
	}

	const edited = "edited on B by hand\n"
	p.b.Box.WriteHome(".codex/AGENTS.md", edited)
	v2 := p.commitAgents(french)
	p.applyToB("codex")
	if got := p.b.Box.ReadHome(".codex/AGENTS.md"); got != edited {
		t.Errorf("an apply without --force overwrote the edit on B: %q", got)
	}
	st := p.stateOfB("codex")
	if !strings.Contains(driftOf(st), "AGENTS.md") || !st.Partial {
		t.Errorf("status = drift [%s] partial=%t, want AGENTS.md and partial", driftOf(st), st.Partial)
	}

	p.applyToB("codex", "--force")
	if got := p.b.Box.ReadHome(".codex/AGENTS.md"); got != french {
		t.Errorf("--force left AGENTS.md = %q, want v2's", got)
	}
	if st := p.stateOfB("codex"); st.AppliedVersion != v2.Version || st.AppliedDigest != v2.Digest || st.Partial || len(st.Drift) != 0 {
		t.Errorf("status after --force = v%d %s partial=%t drift [%s], want v%d %s whole", st.AppliedVersion, st.AppliedDigest, st.Partial, driftOf(st), v2.Version, v2.Digest)
	}
}

// TestScenario_E2eV02_DigestEfectivo: a Codex Run on B pinned to v2 of the
// Workflow, with v3 published before it starts, records as
// effective_config_digest the digest of v2's Codex configuration as a Worker
// materialises it, which is not v3's.
func TestScenario_E2eV02_DigestEfectivo(t *testing.T) {
	p := newPhase2(t, Opts{MaxConcurrent: 1})
	p.adopt()
	v2 := p.commitAgents(french)
	want := materializedDigest(t, p.a.Box.Home())

	// Both Workers busy: the Codex Run waits for room, pinned to v2, while v3
	// is published.
	tokens := []string{p.Ledger.Token(), p.Ledger.Token()}
	var created []v1.CreateTaskResponse
	for _, token := range tokens {
		created = append(created, p.Do(p.Ledger.Script(token)))
		waitHarness(p.Cluster, token)
	}
	p.mustCF(p.Ctl, "project", "secrets", "allow", "e2e", codexSecretRef)
	p.mustCF(p.Ctl, "project", "set", "e2e", "--workflow", workflowName+"@latest")
	pinned := JSON[v1.CreateTaskResponse](p.Cluster, p.Ctl, "do", "say ok", "--harness", "codex")
	if r := pinned.Run; r == nil || r.Workflow == nil || r.Workflow.Version != v2.Version || r.Workflow.Digest != v2.Digest {
		t.Fatalf("the Codex Run was created pinned to %+v, want v%d %s", pinned.Run, v2.Version, v2.Digest)
	}
	p.WaitRun(pinned.Run.ID, "queued for room", func(r v1.Run) bool { return r.State == domain.RunQueued && r.QueuedReason != "" })
	v3 := p.commitAgents(german)
	if v3.Version != v2.Version+1 {
		t.Fatalf("v3 = %+v", v3)
	}
	notWant := materializedDigest(t, p.a.Box.Home())
	if notWant == want {
		t.Fatal("v2 and v3 materialise alike: the test would prove nothing")
	}

	for _, token := range tokens {
		p.Ledger.Release(token)
	}
	run := p.WaitRun(pinned.Run.ID, "settled", func(r v1.Run) bool { return settled(r.State) && r.Runtime != domain.RuntimeActive })
	if run.State != domain.RunCompleted || run.WorkerID != p.b.ID {
		t.Fatalf("the Codex Run: %s", RunStage(run))
	}
	shown := p.Run(p.Observer, run.ID)
	if shown.Workflow == nil || shown.Workflow.Version != v2.Version || shown.Workflow.Digest != v2.Digest {
		t.Errorf("cf run show: workflow %+v, want v%d %s", shown.Workflow, v2.Version, v2.Digest)
	}
	switch got := shown.EffectiveConfigDigest; got {
	case want:
	case notWant:
		t.Errorf("effective_config_digest is v3's (%s): the Run was not materialised from the version it is pinned to", got)
	default:
		t.Errorf("effective_config_digest = %q, want %s, the digest of v2 materialised", got, want)
	}

	Check(t, p.Cluster, Expect{Tasks: append(expectedTasks(created, tokens), ExpectedTask{ID: pinned.Task.ID})})
}

// materializedDigest is the effective_config_digest a Run of Codex gets from a
// Workflow made of the Codex configuration under home: the treedigest/v1 of
// that configuration snapshotted and materialised as a Worker does, without
// the auth and with the secret markers unrendered.
func materializedDigest(t *testing.T, home string) string {
	t.Helper()
	h, err := localhost.New(localhost.Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := codex.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mat, err := host.NewMaterializer(h).Materialize(t.Context(), codex.Adapter{}, t.TempDir(), snap)
	if err != nil {
		t.Fatal(err)
	}
	return mat.Digest
}

func parseTOML(t *testing.T, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := toml.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("not TOML: %v\n%s", err, text)
	}
	return v
}
