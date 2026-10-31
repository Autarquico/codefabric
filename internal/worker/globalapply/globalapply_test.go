package globalapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
)

// kit is what differs between adapters: where three kinds of component live
// and how they read. doc is a plain file, dir a directory, sec a file whose
// content carries the secret marker {{cf-secret:tok/a}}.
type kit struct {
	name                 string
	adapter              harness.Adapter
	schema               int
	doc, dir, sec        string // component names
	docPath, dirFile     string // paths relative to the harness root
	secKind              harness.ComponentKind
	secContent           func(v string) string
	secOnHost, docOnHost string // paths under home
}

var kits = []kit{
	{
		name: "claude", adapter: claude.Adapter{}, schema: claude.SchemaVersion,
		doc: claude.MemoryComponent, docPath: "CLAUDE.md",
		dir: claude.CommandsComponent, dirFile: "commands/a.md",
		sec: claude.SettingsComponent, secKind: harness.KindFile,
		secContent: func(v string) string {
			return fmt.Sprintf(`{"env":{"TOKEN":"{{cf-secret:tok/a}}"},"model":%q}`, v)
		},
		secOnHost: ".claude/settings.json", docOnHost: ".claude/CLAUDE.md",
	},
	{
		name: "codex", adapter: codex.Adapter{}, schema: codex.SchemaVersion,
		doc: codex.AgentsComponent, docPath: "AGENTS.md",
		dir: codex.PromptsComponent, dirFile: "prompts/a.md",
		sec: codex.ConfigComponent, secKind: harness.KindFile,
		secContent: func(v string) string {
			return fmt.Sprintf("model = %q\n[mcp_servers.x]\ncommand = \"x\"\n[mcp_servers.x.env]\nTOKEN = \"{{cf-secret:tok/a}}\"\n", v)
		},
		secOnHost: ".codex/config.toml", docOnHost: ".codex/AGENTS.md",
	},
}

func (k kit) file(path, content string) harness.File {
	b := []byte(content)
	return harness.File{Path: path, Mode: 0o600, Content: b, SecretRefs: harness.SecretRefs(b)}
}

func (k kit) component(t *testing.T, name string, kind harness.ComponentKind, own harness.Ownership, files ...harness.File) harness.Component {
	t.Helper()
	d, err := harness.FilesDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	return harness.Component{Name: name, Kind: kind, Ownership: own, Files: files, Digest: d}
}

// snap is the desired configuration at version v of the components named.
func (k kit) snap(t *testing.T, v string, own harness.Ownership, names ...string) harness.NativeSnapshot {
	t.Helper()
	s := harness.NativeSnapshot{Harness: k.adapter.ID(), SchemaVersion: k.schema}
	for _, n := range names {
		switch n {
		case k.doc:
			s.Components = append(s.Components, k.component(t, n, harness.KindFile, own, k.file(k.docPath, "doc "+v+"\n")))
		case k.dir:
			s.Components = append(s.Components, k.component(t, n, harness.KindDir, own, k.file(k.dirFile, "dir "+v+"\n")))
		case k.sec:
			s.Components = append(s.Components, k.component(t, n, k.secKind, own, k.file(k.sec2path(), k.secContent(v))))
		}
	}
	return s
}

func (k kit) sec2path() string { return filepath.Base(k.secOnHost) }

type world struct {
	k    kit
	home string
	data string
	e    *Engine
}

func newWorld(t *testing.T, k kit) *world {
	t.Helper()
	home, data := t.TempDir(), t.TempDir()
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Adapter: k.adapter, Host: h, DataDir: data, Secrets: func(_ context.Context, ref string) ([]byte, error) {
		if ref == "tok/a" {
			return []byte("s3cr3t-value"), nil
		}
		return nil, errors.New("not in the store")
	}}
	return &world{k: k, home: home, data: data, e: e}
}

func (w *world) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.home, rel)) //nolint:gosec // a path under the temporary home of the test
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (w *world) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(w.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dirOnHost is the file of the dir component, under home.
func (k kit) dirOnHost() string { return filepath.Join(filepath.Dir(k.docOnHost), k.dirFile) }

func (w *world) apply(t *testing.T, req Request) (Result, error) {
	t.Helper()
	return w.e.Apply(context.Background(), req)
}

func action(t *testing.T, p Plan, name string) ComponentPlan {
	t.Helper()
	for _, c := range p.Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no component %q in the plan %+v", name, p.Components)
	return ComponentPlan{}
}

func eachKit(t *testing.T, f func(t *testing.T, w *world)) {
	for _, k := range kits {
		t.Run(k.name, func(t *testing.T) { f(t, newWorld(t, k)) })
	}
}

func TestApply_FirstApplyWritesAndRendersSecretsThenIsANoOp(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		req := Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc, k.dir, k.sec), WorkflowID: "wf", Version: "1"}
		res, err := w.apply(t, req)
		if err != nil || !res.Applied {
			t.Fatalf("apply = %+v, %v", res, err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v1\n" {
			t.Fatalf("doc = %q", got)
		}
		sec := w.read(t, k.secOnHost)
		if !strings.Contains(sec, "s3cr3t-value") || strings.Contains(sec, "cf-secret") {
			t.Fatalf("secret not rendered: %q", sec)
		}
		again, err := w.apply(t, req)
		if err != nil || again.Applied || !again.Plan.NoOp {
			t.Fatalf("second apply = %+v, %v; want a no-op", again, err)
		}
		for _, c := range again.Plan.Components {
			if c.Action != ActionPreserve || c.Drift {
				t.Fatalf("component %s = %s drift=%v, want preserve", c.Name, c.Action, c.Drift)
			}
		}
		if _, err := os.Lstat(backupDir(w.e.harnessDir())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup left behind: %v", err)
		}
	})
}

func TestApply_ForceRepairsWhatIsAlreadyApplied(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		req := Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}
		if _, err := w.apply(t, req); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(w.home, k.docOnHost)); err != nil {
			t.Fatal(err)
		}
		if res, err := w.apply(t, req); err != nil || res.Applied {
			t.Fatalf("without force: %+v, %v", res, err)
		}
		req.Force = true
		if res, err := w.apply(t, req); err != nil || !res.Applied {
			t.Fatalf("force: %+v, %v", res, err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v1\n" {
			t.Fatalf("doc = %q", got)
		}
	})
}

func TestApply_SelectiveApplyKeepsTheOthersAndTheirBaseline(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc, k.dir)}); err != nil {
			t.Fatal(err)
		}
		// v2 of both, only doc selected.
		res, err := w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Managed, k.doc, k.dir), Components: []string{k.doc}})
		if err != nil || !res.Applied {
			t.Fatalf("apply = %+v, %v", res, err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v2\n" {
			t.Fatalf("doc = %q", got)
		}
		dirPath := k.dirOnHost()
		if got := w.read(t, dirPath); got != "dir v1\n" {
			t.Fatalf("unselected dir = %q, want v1", got)
		}
		if a := action(t, res.Plan, k.dir); a.Action != ActionPreserve || a.Reason != "not selected" {
			t.Fatalf("dir plan = %+v", a)
		}
		// A snapshot without dir, with only doc selected: dir is not deleted.
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v3", harness.Managed, k.doc), Components: []string{k.doc}}); err != nil {
			t.Fatal(err)
		}
		if got := w.read(t, dirPath); got != "dir v1\n" {
			t.Fatalf("unselected dir deleted or changed: %q", got)
		}
		// Selecting it now does delete it, and drops its baseline.
		res, err = w.apply(t, Request{Snapshot: k.snap(t, "v3", harness.Managed, k.doc), Components: []string{k.dir}})
		if err != nil || !res.Applied {
			t.Fatalf("delete = %+v, %v", res, err)
		}
		if got := w.read(t, dirPath); got != "<absent>" {
			t.Fatalf("dir = %q, want deleted", got)
		}
		bl, err := readBaseline(w.e.harnessDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := bl.Components[k.dir]; ok || bl.Components[k.doc].Desired == "" {
			t.Fatalf("baseline = %+v", bl.Components)
		}
	})
}

func TestApply_UnknownComponentIsRefusedBeforeAnyWrite(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		_, err := w.apply(t, Request{Snapshot: w.k.snap(t, "v1", harness.Managed, w.k.doc), Components: []string{"nope"}})
		if err == nil {
			t.Fatal("want an error")
		}
		if got := w.read(t, w.k.docOnHost); got != "<absent>" {
			t.Fatalf("doc = %q", got)
		}
	})
}

func TestApply_ALocalComponentIsNeverOverwritten(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		w.write(t, k.docOnHost, "mine\n")
		for _, force := range []bool{false, true} {
			res, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Local, k.doc), Force: force})
			if err != nil || res.Applied {
				t.Fatalf("force=%v: %+v, %v", force, res, err)
			}
			if got := w.read(t, k.docOnHost); got != "mine\n" {
				t.Fatalf("force=%v: doc = %q", force, got)
			}
			if a := action(t, res.Plan, k.doc); a.Action != ActionPreserve || a.Origin != OriginLocal || !a.Drift {
				t.Fatalf("force=%v: plan = %+v", force, a)
			}
		}
	})
}

func TestApply_ManagedDriftNeedsForce(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}); err != nil {
			t.Fatal(err)
		}
		w.write(t, k.docOnHost, "edited by hand\n")
		req := Request{Snapshot: k.snap(t, "v2", harness.Managed, k.doc)}
		res, err := w.apply(t, req)
		if err != nil || res.Applied {
			t.Fatalf("apply = %+v, %v", res, err)
		}
		if a := action(t, res.Plan, k.doc); a.Action != ActionSkipDrift || !a.Drift {
			t.Fatalf("plan = %+v", a)
		}
		if got := w.read(t, k.docOnHost); got != "edited by hand\n" {
			t.Fatalf("doc = %q", got)
		}
		req.Force = true
		if res, err := w.apply(t, req); err != nil || !res.Applied {
			t.Fatalf("force = %+v, %v", res, err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v2\n" {
			t.Fatalf("doc = %q", got)
		}
	})
}

func TestApply_AMissingSecretFailsTheWholeApplyWithoutWrites(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		w.e.Secrets = func(context.Context, string) ([]byte, error) { return nil, errors.New("not in the store") }
		_, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc, k.sec)})
		if !errors.Is(err, harness.ErrSecretUnavailable) {
			t.Fatalf("err = %v", err)
		}
		if got := w.read(t, k.docOnHost); got != "<absent>" {
			t.Fatalf("doc written: %q", got)
		}
		if got := w.read(t, k.secOnHost); got != "<absent>" {
			t.Fatalf("secret file written: %q", got)
		}
		if _, err := os.Lstat(baselinePath(w.e.harnessDir())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("baseline written: %v", err)
		}
		w.e.Secrets = nil
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc, k.sec)}); !errors.Is(err, harness.ErrSecretUnavailable) {
			t.Fatalf("no store: err = %v", err)
		}
	})
}

func TestApply_ACrashMidApplyIsRestoredByRecover(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}); err != nil {
			t.Fatal(err)
		}
		baseBefore, err := os.ReadFile(baselinePath(w.e.harnessDir()))
		if err != nil {
			t.Fatal(err)
		}
		type crash struct{}
		w.e.hooks.afterApply = func() { panic(crash{}) }
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("the hook did not fire")
				}
			}()
			_, _ = w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Managed, k.doc, k.dir)})
		}()
		if got := w.read(t, k.docOnHost); got != "doc v2\n" {
			t.Fatalf("the crash left doc = %q, want the half-applied v2", got)
		}
		if err := Recover(w.data); err != nil {
			t.Fatal(err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v1\n" {
			t.Fatalf("doc after recover = %q", got)
		}
		dirPath := k.dirOnHost()
		if got := w.read(t, dirPath); got != "<absent>" {
			t.Fatalf("dir after recover = %q, want absent", got)
		}
		baseAfter, _ := os.ReadFile(baselinePath(w.e.harnessDir()))
		if string(baseAfter) != string(baseBefore) {
			t.Fatalf("baseline changed:\n%s\n%s", baseBefore, baseAfter)
		}
		if _, err := os.Lstat(backupDir(w.e.harnessDir())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup left after recover: %v", err)
		}
		// And the host is a clean base for the next apply.
		w.e.hooks.afterApply = nil
		if res, err := w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Managed, k.doc)}); err != nil || !res.Applied {
			t.Fatalf("reapply = %+v, %v", res, err)
		}
	})
}

func TestApply_APreviewThatChangedIsRefused(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		req := Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}
		preview, err := w.apply(t, Request{Snapshot: req.Snapshot, DryRun: true})
		if err != nil || preview.Applied {
			t.Fatalf("dry run = %+v, %v", preview, err)
		}
		if got := w.read(t, k.docOnHost); got != "<absent>" {
			t.Fatalf("dry run wrote: %q", got)
		}
		w.write(t, k.docOnHost, "appeared after the preview\n")
		req.ExpectedPreview = preview.Plan.Digest
		if _, err := w.apply(t, req); !errors.Is(err, ErrPreviewChanged) {
			t.Fatalf("err = %v", err)
		}
		if got := w.read(t, k.docOnHost); got != "appeared after the preview\n" {
			t.Fatalf("doc = %q", got)
		}
		// Without Force the new content is drift: nothing is written.
		req.ExpectedPreview = ""
		if res, err := w.apply(t, req); err != nil || res.Applied || action(t, res.Plan, k.doc).Action != ActionSkipDrift {
			t.Fatalf("first apply over other content = %+v, %v", res, err)
		}
		// A fresh preview, forced, applies.
		req.Force = true
		p, _ := w.e.Plan(context.Background(), req)
		req.ExpectedPreview = p.Digest
		if res, err := w.apply(t, req); err != nil || !res.Applied {
			t.Fatalf("apply = %+v, %v", res, err)
		}
	})
}

func TestPlan_SaysOriginOwnershipPathsActionAndNoSecretValues(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		w.write(t, k.docOnHost, "mine\n")
		req := Request{Snapshot: k.snap(t, "v1", harness.Managed, k.dir, k.sec), Version: "1"}
		p, err := w.e.Plan(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if a := action(t, p, k.dir); a.Action != ActionWrite || a.Origin != OriginAbsent || a.Ownership != harness.Managed || len(a.Paths) != 1 {
			t.Fatalf("dir = %+v", a)
		}
		s := action(t, p, k.sec)
		if len(s.Refs) != 1 || s.Refs[0] != "tok/a" {
			t.Fatalf("sec refs = %v", s.Refs)
		}
		for _, v := range s.Values {
			if strings.Contains(v, "s3cr3t-value") || !strings.Contains(v, "{{cf-secret:tok/a}}") {
				t.Fatalf("values = %q", v)
			}
		}
		if a := action(t, p, k.doc); a.Origin != OriginLocal || a.Action != ActionPreserve || len(a.Values) != 0 || len(a.Refs) != 0 {
			t.Fatalf("host-only doc = %+v", a)
		}
		if b, _ := json.Marshal(p); strings.Contains(string(b), "mine") {
			t.Fatalf("the content of the host leaked into the plan: %s", b)
		}
		if _, err := w.apply(t, req); err != nil {
			t.Fatal(err)
		}
		p, _ = w.e.Plan(context.Background(), req)
		if a := action(t, p, k.dir); a.Origin != OriginWorkflow || a.Version != "1" || a.Action != ActionPreserve {
			t.Fatalf("applied dir = %+v", a)
		}
	})
}

func TestApply_ACedComponentIsKeptAndNeverDeletedLater(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}); err != nil {
			t.Fatal(err)
		}
		res, err := w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Local, k.doc)})
		if err != nil || !res.Applied {
			t.Fatalf("cede = %+v, %v", res, err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v1\n" {
			t.Fatalf("doc = %q", got)
		}
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v3", harness.Managed, k.dir)}); err != nil {
			t.Fatal(err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v1\n" {
			t.Fatalf("ceded doc deleted: %q", got)
		}
	})
}

func TestApply_AComponentAlreadyAsTheWorkflowHasItIsAdoptedAndNextVersionIsNotDrift(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		w.write(t, k.docOnHost, "doc v1\n")
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}); err != nil {
			t.Fatal(err)
		}
		if got := w.read(t, k.docOnHost); got != "doc v1\n" {
			t.Fatalf("adoption changed the host: %q", got)
		}
		res, err := w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Managed, k.doc)})
		if err != nil || !res.Applied {
			t.Fatalf("v2 = %+v, %v", res, err)
		}
		if a := action(t, res.Plan, k.doc); a.Drift || a.Action != ActionWrite {
			t.Fatalf("plan = %+v, want a write with no drift", a)
		}
		if got := w.read(t, k.docOnHost); got != "doc v2\n" {
			t.Fatalf("doc = %q", got)
		}
	})
}

func TestPlan_ALocalComponentDriftsFromTheWorkflowEvenIfItWasNeverApplied(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		other := map[string]string{"claude": `{"model":"mine"}`, "codex": "model = \"mine\"\n"}[k.name]
		w.write(t, k.secOnHost, other)
		res, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Local, k.sec)})
		if err != nil || res.Applied {
			t.Fatalf("apply = %+v, %v", res, err)
		}
		if a := action(t, res.Plan, k.sec); !a.Drift || a.Action != ActionPreserve || a.Origin != OriginLocal {
			t.Fatalf("plan = %+v", a)
		}
	})
}

func TestApply_AComponentCededToLocalStillReportsDriftFromTheWorkflow(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.sec)}); err != nil {
			t.Fatal(err)
		}
		res, err := w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Local, k.sec)})
		if err != nil || !res.Applied {
			t.Fatalf("cede = %+v, %v", res, err)
		}
		if a := action(t, res.Plan, k.sec); !a.Drift || a.Action != ActionPreserve {
			t.Fatalf("plan = %+v", a)
		}
		again, err := w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Local, k.sec)})
		if err != nil || again.Applied {
			t.Fatalf("again = %+v, %v", again, err)
		}
		if a := action(t, again.Plan, k.sec); !a.Drift {
			t.Fatalf("plan after the cession = %+v, want drift", a)
		}
	})
}

func TestPlan_ALocalMCPServerDriftsFromTheWorkflowWithoutBeingApplied(t *testing.T) {
	w := newWorld(t, kits[0])
	w.write(t, ".claude.json", `{"mcpServers": {"x": {"command": "mine"}}}`)
	want := []byte(`{"mcpServers": {"x": {"command": "team"}}}`)
	files := []harness.File{{Path: claude.StateFile, Mode: 0o600, Content: want}}
	d, err := harness.FilesDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	snap := harness.NativeSnapshot{Harness: claude.ID, SchemaVersion: claude.SchemaVersion, Components: []harness.Component{
		{Name: claude.MCPComponent, Kind: harness.KindJSONSubset, Ownership: harness.Local, Files: files, Digest: d},
	}}
	res, err := w.apply(t, Request{Snapshot: snap})
	if err != nil || res.Applied {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if a := action(t, res.Plan, claude.MCPComponent); !a.Drift || a.Action != ActionPreserve {
		t.Fatalf("plan = %+v", a)
	}
	if got := w.read(t, ".claude.json"); !strings.Contains(got, "mine") {
		t.Fatalf(".claude.json = %q", got)
	}
}

// stamp is what a write would change: the inode and mtime of a path.
type stamp struct {
	ino   uint64
	mtime int64
}

func (w *world) stamp(t *testing.T, rel string) stamp {
	t.Helper()
	fi, err := os.Stat(filepath.Join(w.home, rel))
	if err != nil {
		t.Fatal(err)
	}
	return stamp{fi.Sys().(*syscall.Stat_t).Ino, fi.ModTime().UnixNano()}
}

// Adopting what the host already has records ownership and baseline and writes
// nothing: no file replaced, no directory deleted and recreated, secret
// markers included.
func TestApply_AdoptingAnIdenticalComponentWritesNothing(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		old := time.Now().Add(-time.Hour).Truncate(time.Second)
		onHost := map[string]string{k.docOnHost: "doc v1\n", k.dirOnHost(): "dir v1\n"}
		names := []string{k.doc, k.dir}
		// Claude reads settings.json back with generated marker refs, so a
		// file carrying tok/a is never "identical" there; codex's config.toml is.
		if k.name == "codex" {
			onHost[k.secOnHost] = k.secContent("v1")
			names = append(names, k.sec)
		}
		for rel, content := range onHost {
			w.write(t, rel, content)
			if err := os.Chtimes(filepath.Join(w.home, rel), old, old); err != nil {
				t.Fatal(err)
			}
		}
		dirRel := filepath.Dir(k.dirOnHost())
		watched := []string{k.docOnHost, k.dirOnHost(), dirRel}
		if len(names) == 3 {
			watched = append(watched, k.secOnHost)
		}
		before := map[string]stamp{}
		for _, rel := range watched {
			before[rel] = w.stamp(t, rel)
		}

		res, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, names...), WorkflowID: "wf", Version: "1"})
		if err != nil || !res.Applied {
			t.Fatalf("adopt = %+v, %v", res, err)
		}
		for _, rel := range watched {
			if got := w.stamp(t, rel); got != before[rel] {
				t.Errorf("%s was written: %+v -> %+v", rel, before[rel], got)
			}
		}
		for rel, content := range onHost {
			if got := w.read(t, rel); got != content {
				t.Errorf("%s = %q, want %q", rel, got, content)
			}
		}

		// Adopted means owned with a baseline: the next version is a write, not drift.
		res, err = w.apply(t, Request{Snapshot: k.snap(t, "v2", harness.Managed, names...), WorkflowID: "wf", Version: "2"})
		if err != nil || !res.Applied {
			t.Fatalf("v2 = %+v, %v", res, err)
		}
		for _, name := range names {
			if a := action(t, res.Plan, name); a.Drift || a.Action != ActionWrite {
				t.Errorf("%s: plan = %+v, want a write with no drift", name, a)
			}
		}
		if got := w.read(t, k.docOnHost); got != "doc v2\n" {
			t.Errorf("doc = %q", got)
		}
	})
}
