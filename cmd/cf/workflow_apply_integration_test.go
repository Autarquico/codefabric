//go:build integration && (linux || darwin)

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// applyFixture is a Controller holding the Workflow "daily" (v1, adopted from
// the person's ~/.codex) and a Worker "alpha" whose HOME, the box, is another
// directory: what an apply writes there is what the Worker's machine holds.
type applyFixture struct {
	src, box string // HOMEs: where the person adopts from, where the Worker applies to
	v1       v1.WorkflowVersion
}

// newApplyFixture adopts daily from a codex HOME and brings up a Worker with
// codex and an empty HOME. The CLI is left pointing at the Controller, with the
// HOME of the Worker (the Worker reads its HOME as it starts); asSource() gives
// the person's HOME back to commit.
func newApplyFixture(t *testing.T, adoptExtra ...string) *applyFixture {
	t.Helper()
	host, _ := bringUpController(t)
	f := &applyFixture{src: codexHome(t, codexConfig)}
	f.v1 = ok[v1.WorkflowVersion](t, adoptArgs("daily", adoptExtra...)...)

	invite := workerInvite(t, host)
	boxMachine := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	f.box = t.TempDir()
	writeCodex(t, filepath.Join(f.box, ".codex"), "auth.json", `{"tokens":{"access_token":"`+authSecret+`"}}`) // logged in: the doctor says online
	t.Setenv("HOME", f.box)
	t.Setenv("CODEX_HOME", "")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: boxMachine, Interval: 100 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), Runtime: &idleRuntime{},
			Harnesses: []harness.Adapter{installedCodex{}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	soon(t, "the Worker to be online", func() bool { return showWorker(t, host, "alpha").State == domain.WorkerOnline })
	use(t, host)
	return f
}

// installedCodex is the codex adapter on a machine that has codex: the doctor
// of the Worker finds it installed whatever the machine running the tests has.
// Detect and Version are answered without a binary.
type installedCodex struct{ codex.Adapter }

func (installedCodex) Detect(context.Context, harness.Host) (harness.Detection, error) {
	return harness.Detection{Installed: true, Path: "/usr/bin/true"}, nil
}

func (installedCodex) Version(context.Context, harness.Host) (string, error) { return "0.0.0", nil }

func (f *applyFixture) asSource(t *testing.T) { t.Helper(); t.Setenv("HOME", f.src) }

// commitAgents commits a v2 whose AGENTS.md says text.
func (f *applyFixture) commitAgents(t *testing.T, text string) v1.WorkflowVersion {
	t.Helper()
	f.asSource(t)
	defer t.Setenv("HOME", f.box)
	writeCodex(t, filepath.Join(f.src, ".codex"), "AGENTS.md", text)
	return ok[v1.WorkflowVersion](t, commitArgs("daily")...)
}

func (f *applyFixture) path(rel string) string { return filepath.Join(f.box, ".codex", rel) }

func (f *applyFixture) read(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(f.path(rel))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(raw)
}

func (f *applyFixture) edit(t *testing.T, rel, text string) {
	t.Helper()
	writeCodex(t, filepath.Join(f.box, ".codex"), rel, text)
}

func applyOn(verb string, extra ...string) []string {
	args := []string{"workflow", verb}
	if verb != "rollback" && verb != "status" {
		args = append(args, "daily@latest")
	}
	return append(args, append([]string{"--worker", "alpha", "--harness", "codex"}, extra...)...)
}

func workflowState(t *testing.T) v1.WorkflowState {
	t.Helper()
	states := ok[v1.WorkflowStateResponse](t, "workflow", "status", "--worker", "alpha", "--harness", "codex").States
	if len(states) != 1 {
		t.Fatalf("status = %+v, want one state", states)
	}
	return states[0]
}

func planOf(t *testing.T, r v1.ApplyWorkflowResponse) v1.WorkflowApplyPlan {
	t.Helper()
	if r.Apply == nil || len(r.Apply.Result) == 0 {
		t.Fatalf("no result in %+v", r)
	}
	var p v1.WorkflowApplyPlan
	if err := json.Unmarshal(r.Apply.Result, &p); err != nil {
		t.Fatalf("result %s: %v", r.Apply.Result, err)
	}
	return p
}

func driftNames(s v1.WorkflowState) []string {
	var names []string
	for _, d := range s.Drift {
		names = append(names, d.Component)
	}
	return names
}

const (
	spanish = "Always answer in Spanish.\n"
	french  = "Always answer in French.\n"
)

// Drift local: a local component edited on the Worker is never overwritten
// (not even with --force) and status shows the drift; a managed one edited is
// skipped without --force and overwritten with it.
func TestScenario_WorkflowApplyDriftLockA_DriftLocal(t *testing.T) {
	f := newApplyFixture(t, "--local", "codex/skills")
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if got := f.read(t, "AGENTS.md"); got != spanish {
		t.Fatalf("first apply left AGENTS.md = %q", got)
	}

	// Local component: edited on the Worker.
	f.edit(t, "skills/review/SKILL.md", "my own skill\n")
	forced := ok[v1.ApplyWorkflowResponse](t, applyOn("apply", "--force")...)
	if got := f.read(t, "skills/review/SKILL.md"); got != "my own skill\n" {
		t.Errorf("a local component was overwritten: %q", got)
	}
	if forced.Apply == nil {
		t.Errorf("--force over an edited Worker should be an order, got %+v", forced)
	}
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if got := f.read(t, "skills/review/SKILL.md"); got != "my own skill\n" {
		t.Errorf("a local component was overwritten without force: %q", got)
	}
	if st := workflowState(t); !strings.Contains(strings.Join(driftNames(st), ","), "skills") {
		t.Errorf("status drift = %+v, want skills", st.Drift)
	}

	// Managed component: edited, then a v2 arrives.
	f.edit(t, "AGENTS.md", "edited by hand\n")
	v2 := f.commitAgents(t, french)
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if got := f.read(t, "AGENTS.md"); got != "edited by hand\n" {
		t.Errorf("a managed component with drift was overwritten without --force: %q", got)
	}
	st := workflowState(t)
	if !strings.Contains(strings.Join(driftNames(st), ","), "AGENTS.md") {
		t.Errorf("status drift = %+v, want AGENTS.md", st.Drift)
	}
	if !st.Partial {
		t.Errorf("status = %+v, want partial after skipping AGENTS.md", st)
	}
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply", "--force")...)
	if got := f.read(t, "AGENTS.md"); got != french {
		t.Errorf("--force left AGENTS.md = %q, want the workflow's", got)
	}
	if got := f.read(t, "skills/review/SKILL.md"); got != "my own skill\n" {
		t.Errorf("--force overwrote a local component: %q", got)
	}
	if st := workflowState(t); st.AppliedVersion != v2.Version || st.AppliedDigest != v2.Digest || st.Partial {
		t.Errorf("status = v%d %s partial=%t, want v%d %s complete", st.AppliedVersion, st.AppliedDigest, st.Partial, v2.Version, v2.Digest)
	}
}

// Dry-run: --dry-run shows the diff, writes no file and leaves the applied
// digest as it was.
func TestScenario_WorkflowApplyDriftLockA_DryRun(t *testing.T) {
	f := newApplyFixture(t)

	// Nothing applied yet.
	code, out, errOut := cf(t, "", applyOn("apply", "--dry-run")...)
	if code != 0 || !strings.Contains(out, "AGENTS.md") {
		t.Fatalf("dry-run: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if _, err := os.Stat(f.path("AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("a dry-run wrote AGENTS.md (%v)", err)
	}
	if st := workflowState(t); st.AppliedDigest != "" || st.AppliedVersion != 0 {
		t.Errorf("a dry-run set the applied state: %+v", st)
	}

	// v1 applied, v2 dry-run.
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	before := workflowState(t)
	if before.AppliedDigest != f.v1.Digest {
		t.Fatalf("applied = %s, want %s", before.AppliedDigest, f.v1.Digest)
	}
	f.commitAgents(t, french)
	dry := ok[v1.ApplyWorkflowResponse](t, applyOn("apply", "--dry-run")...)
	if p := planOf(t, dry); p.Applied {
		t.Errorf("a dry-run says it applied: %+v", p)
	}
	code, out, errOut = cf(t, "", applyOn("apply", "--dry-run")...)
	if code != 0 || !strings.Contains(out, "AGENTS.md") {
		t.Errorf("dry-run output does not show the diff: exit %d\n%s\n%s", code, out, errOut)
	}
	if got := f.read(t, "AGENTS.md"); got != spanish {
		t.Errorf("a dry-run changed AGENTS.md: %q", got)
	}
	if after := workflowState(t); after.AppliedDigest != before.AppliedDigest || after.AppliedVersion != before.AppliedVersion {
		t.Errorf("applied digest moved from %s v%d to %s v%d", before.AppliedDigest, before.AppliedVersion, after.AppliedDigest, after.AppliedVersion)
	}
}

// No-op: applying the digest the Worker already holds writes nothing and
// leaves the applied digest as it was. The Controller does not decide it: the
// order is made and the Worker reports a plan that is a no-op.
func TestScenario_WorkflowApplyDriftLockA_NoOp(t *testing.T) {
	f := newApplyFixture(t)
	first := ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if first.Apply == nil || first.Apply.State != v1.ApplyStateApplied {
		t.Fatalf("first apply = %+v", first)
	}
	if f.read(t, "AGENTS.md") != spanish {
		t.Fatal("the first apply wrote nothing")
	}
	before := workflowState(t)
	stat := func() os.FileInfo {
		info, err := os.Stat(f.path("AGENTS.md"))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	written := stat().ModTime()

	again := ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if again.Apply == nil || again.Apply.State != v1.ApplyStateApplied || !planOf(t, again).NoOp {
		t.Errorf("second apply = %+v, want an applied order whose plan is a no-op", again)
	}
	if got := stat().ModTime(); !got.Equal(written) || f.read(t, "AGENTS.md") != spanish {
		t.Errorf("the repeated apply wrote AGENTS.md (mtime %v, was %v)", got, written)
	}
	if st := workflowState(t); st.AppliedDigest != f.v1.Digest || st.AppliedVersion != before.AppliedVersion || st.PreviousVersion != before.PreviousVersion || len(st.Drift) != 0 {
		t.Errorf("the state moved: %+v (was %+v)", st, before)
	}
	if _, out, _ := cf(t, "", applyOn("apply")...); !strings.Contains(out, "No changes") {
		t.Errorf("human output = %q, want it to say there are no changes", out)
	}
}

// A local edit made after the last apply is seen by the next normal apply
// (no force, no earlier operation): the Controller's record can be stale, the
// Worker's fresh plan is not.
func TestScenario_WorkflowApplyDriftLockA_EditThenNormalApplyShowsTheDrift(t *testing.T) {
	f := newApplyFixture(t)
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if st := workflowState(t); len(st.Drift) != 0 {
		t.Fatalf("drift before the edit = %+v", st.Drift)
	}
	f.edit(t, "AGENTS.md", "edited by hand\n")
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if got := f.read(t, "AGENTS.md"); got != "edited by hand\n" {
		t.Errorf("a managed edit was overwritten without --force: %q", got)
	}
	if st := workflowState(t); !strings.Contains(strings.Join(driftNames(st), ","), "AGENTS.md") {
		t.Errorf("status drift = %+v, want AGENTS.md", st.Drift)
	}
}

// Explain: before applying, explain says per component its origin, ownership
// and the action, and writes nothing.
func TestScenario_WorkflowApplyDriftLockA_Explain(t *testing.T) {
	f := newApplyFixture(t, "--local", "codex/skills")

	r := ok[v1.ApplyWorkflowResponse](t, applyOn("explain")...)
	byName := map[string]v1.WorkflowComponentPlan{}
	for _, c := range planOf(t, r).Components {
		byName[c.Name] = c
	}
	agents, skills := byName["AGENTS.md"], byName["skills"]
	if agents.Origin != "absent" || agents.Ownership != "managed" || agents.Action != "write" || len(agents.Paths) == 0 {
		t.Errorf("AGENTS.md = %+v, want absent (nothing on the Worker yet)/managed/write with paths", agents)
	}
	if skills.Ownership != "local" || skills.Action == "write" {
		t.Errorf("skills = %+v, want local and not written", skills)
	}
	_, out, _ := cf(t, "", applyOn("explain")...)
	for _, want := range []string{"AGENTS.md", "origin", "ownership", "action"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(f.path("AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("explain wrote on the Worker (%v)", err)
	}
	if st := workflowState(t); st.AppliedDigest != "" {
		t.Errorf("explain set the applied state: %+v", st)
	}
}

// --components touches only the chosen component.
func TestScenario_WorkflowApplyDriftLockA_SelectiveComponents(t *testing.T) {
	f := newApplyFixture(t)
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply", "--components", "AGENTS.md")...)
	if got := f.read(t, "AGENTS.md"); got != spanish {
		t.Errorf("AGENTS.md = %q", got)
	}
	for _, rel := range []string{"config.toml", "skills/review/SKILL.md"} {
		if _, err := os.Stat(f.path(rel)); !os.IsNotExist(err) {
			t.Errorf("%s was written by an apply of AGENTS.md alone (%v)", rel, err)
		}
	}
}

// Rollback reapplies the version held before the current one.
func TestScenario_WorkflowApplyDriftLockA_Rollback(t *testing.T) {
	f := newApplyFixture(t)
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	v2 := f.commitAgents(t, french)
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if got := f.read(t, "AGENTS.md"); got != french {
		t.Fatalf("v2 left AGENTS.md = %q", got)
	}

	rolled := ok[v1.ApplyWorkflowResponse](t, applyOn("rollback")...)
	if rolled.Apply == nil || rolled.Apply.Mode != v1.ApplyModeRollback || rolled.Apply.Version != f.v1.Version {
		t.Errorf("rollback order = %+v", rolled.Apply)
	}
	if got := f.read(t, "AGENTS.md"); got != spanish {
		t.Errorf("rollback left AGENTS.md = %q, want v1's", got)
	}
	if st := workflowState(t); st.AppliedVersion != f.v1.Version || st.AppliedDigest != f.v1.Digest || st.PreviousVersion != v2.Version {
		t.Errorf("status after rollback = v%d %s previous v%d", st.AppliedVersion, st.AppliedDigest, st.PreviousVersion)
	}
}
