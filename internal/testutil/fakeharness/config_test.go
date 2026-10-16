package fakeharness_test

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

func writeHost(t *testing.T, host testHost, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := host.path(rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readHost(t *testing.T, host testHost, rel string) string {
	t.Helper()
	b, err := os.ReadFile(host.path(rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return m
}

func component(name string, kind harness.ComponentKind, own harness.Ownership, files map[string]string) harness.Component {
	c := harness.Component{Name: name, Kind: kind, Ownership: own}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		c.Files = append(c.Files, harness.File{Path: p, Mode: 0o600, Content: []byte(files[p])})
	}
	return c
}

func snapshot(cs ...harness.Component) harness.NativeSnapshot {
	return harness.NativeSnapshot{Harness: fakeharness.DefaultID, SchemaVersion: fakeharness.SchemaVersion, Components: cs}
}

func changes(cs []harness.Change) map[string]harness.ChangeKind {
	m := map[string]harness.ChangeKind{}
	for _, c := range cs {
		m[c.Name] = c.Kind
	}
	return m
}

func TestSnapshotConfig_ReadsEveryComponentAndNeverTheAuth(t *testing.T) {
	host := newHost(t)
	writeHost(t, host, map[string]string{
		"settings.json":        `{"theme":"dark"}`,
		"fakeharness.json":     `{"steps":[]}`,
		"skills/a/SKILL.md":    "a",
		"skills/b/ref/note.md": "b",
		"credentials.json":     "token",
	})
	snap, err := fakeharness.Adapter{}.SnapshotConfig(t.Context(), host, harness.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range snap.Components {
		names = append(names, c.Name)
		if c.Ownership != harness.Managed {
			t.Errorf("%s: ownership %s, want managed", c.Name, c.Ownership)
		}
		for _, f := range c.Files {
			if f.Path == fakeharness.AuthFile {
				t.Errorf("%s carries the auth", c.Name)
			}
		}
		if want, _ := harness.FilesDigest(c.Files); c.Digest != want {
			t.Errorf("%s: digest %s, want %s", c.Name, c.Digest, want)
		}
	}
	if want := []string{"script", "settings", "skills/a", "skills/b"}; !slices.Equal(names, want) {
		t.Errorf("components = %v, want %v", names, want)
	}

	only, err := fakeharness.Adapter{}.SnapshotConfig(t.Context(), host, harness.SnapshotOptions{Components: []string{"skills/b"}})
	if err != nil || len(only.Components) != 1 || only.Components[0].Files[0].Path != "skills/b/ref/note.md" {
		t.Errorf("snapshot of skills/b = %+v, %v", only, err)
	}
	if _, err := (fakeharness.Adapter{}).SnapshotConfig(t.Context(), host, harness.SnapshotOptions{Components: []string{"skills/z"}}); err == nil {
		t.Error("a snapshot of a missing component = nil error")
	}
}

func TestApplyConfig_TargetRunWritesTheSnapshotAloneAndLinksTheAuth(t *testing.T) {
	host := newHost(t)
	writeHost(t, host, map[string]string{"credentials.json": "token", "settings.json": `{"host":"only"}`})
	a := fakeharness.Adapter{Caps: allCaps}
	snap := snapshot(
		component("settings", harness.KindJSONSubset, harness.Managed, map[string]string{"settings.json": `{"theme":"dark"}`}),
		component("skills/a", harness.KindDir, harness.Local, map[string]string{"skills/a/SKILL.md": "a"}),
	)
	dir := t.TempDir()

	dry, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Fatalf("a dry run wrote %v", entries)
	}

	res, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, dry) {
		t.Errorf("apply = %+v, its dry run said %+v", res, dry)
	}
	if !slices.Equal(res.AuthLinks, []string{fakeharness.AuthFile}) {
		t.Errorf("auth links = %v", res.AuthLinks)
	}
	if got := changes(res.Changes); !reflect.DeepEqual(got, map[string]harness.ChangeKind{"settings": harness.Added, "skills/a": harness.Added}) {
		t.Errorf("changes = %v", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "settings.json")); string(got) != `{"theme":"dark"}` { //nolint:gosec // under the temp dir
		t.Errorf("settings.json = %q: a TargetRun is the snapshot alone, nothing of the host", got)
	}
	if target, err := os.Readlink(filepath.Join(dir, fakeharness.AuthFile)); err != nil || target != host.path(fakeharness.AuthFile) {
		t.Errorf("credentials.json links to %q, %v; want the auth of the host", target, err)
	}
	if _, err := treedigest.Dir(dir, treedigest.Options{ApprovedLinks: res.AuthLinks}); err != nil {
		t.Errorf("the run config does not digest with its auth links: %v", err)
	}

	if _, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}}); err == nil {
		t.Error("an apply into a directory that is not empty = nil error")
	}
	unlinked := t.TempDir()
	res, err = a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: unlinked}})
	if err != nil || res.AuthLinks != nil {
		t.Errorf("without LinkAuth: %+v, %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(unlinked, fakeharness.AuthFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("without LinkAuth the auth is there: %v", err)
	}
}

// TestApplyConfig_TargetGlobalKeepsWhatIsNotManaged is the ownership fixture
// of the port: a managed component, a local one the person changed, a JSON
// with fields of its own and the auth apart.
func TestApplyConfig_TargetGlobalKeepsWhatIsNotManaged(t *testing.T) {
	host := newHost(t)
	writeHost(t, host, map[string]string{
		"settings.json":         `{"theme":"light","editor":"vim"}`,
		"skills/mine/SKILL.md":  "mine, edited by hand",
		"skills/stray/SKILL.md": "never applied",
		"credentials.json":      "token",
	})
	a := fakeharness.Adapter{}
	snap := snapshot(
		component("settings", harness.KindJSONSubset, harness.Managed, map[string]string{"settings.json": `{"theme":"dark"}`}),
		component("skills/a", harness.KindDir, harness.Managed, map[string]string{"skills/a/SKILL.md": "a"}),
		component("skills/mine", harness.KindDir, harness.Local, map[string]string{"skills/mine/SKILL.md": "mine"}),
	)
	res, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(res.Changes); !reflect.DeepEqual(got, map[string]harness.ChangeKind{"settings": harness.Modified, "skills/a": harness.Added}) {
		t.Errorf("changes = %v", got)
	}
	if got := jsonOf(t, readHost(t, host, "settings.json")); !reflect.DeepEqual(got, map[string]any{"theme": "dark", "editor": "vim"}) {
		t.Errorf("settings.json = %v: the managed field set and the others kept", got)
	}
	for rel, want := range map[string]string{
		"skills/a/SKILL.md":     "a",
		"skills/mine/SKILL.md":  "mine, edited by hand",
		"skills/stray/SKILL.md": "never applied",
		"credentials.json":      "token",
	} {
		if got := readHost(t, host, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}

	// A second snapshot without skills/a and settings removes what was
	// applied, and nothing else.
	res, err = a.ApplyConfig(t.Context(), host, snapshot(), harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(res.Changes); !reflect.DeepEqual(got, map[string]harness.ChangeKind{"settings": harness.Removed, "skills/a": harness.Removed}) {
		t.Errorf("changes = %v", got)
	}
	if _, err := os.Stat(host.path("skills/a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("skills/a is still there: %v", err)
	}
	if got := jsonOf(t, readHost(t, host, "settings.json")); !reflect.DeepEqual(got, map[string]any{"editor": "vim"}) {
		t.Errorf("settings.json = %v: only the applied field goes", got)
	}
	for _, rel := range []string{"skills/mine/SKILL.md", "skills/stray/SKILL.md", "credentials.json"} {
		if _, err := os.Stat(host.path(rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

func TestApplyConfig_AComponentCededToThePersonIsNeverRemoved(t *testing.T) {
	host := newHost(t)
	a := fakeharness.Adapter{}
	for _, owner := range []harness.Ownership{harness.Managed, harness.Local} {
		snap := snapshot(component("skills/a", harness.KindDir, owner, map[string]string{"skills/a/SKILL.md": "a"}))
		if _, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetGlobal{}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := a.ApplyConfig(t.Context(), host, snapshot(), harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) > 0 {
		t.Errorf("changes = %v", changes(res.Changes))
	}
	if got := readHost(t, host, "skills/a/SKILL.md"); got != "a" {
		t.Errorf("skills/a/SKILL.md = %q", got)
	}
}

func TestApplyConfig_TargetGlobalDryRunChangesNothing(t *testing.T) {
	host := newHost(t)
	snap := snapshot(component("skills/a", harness.KindDir, harness.Managed, map[string]string{"skills/a/SKILL.md": "a"}))
	res, err := fakeharness.Adapter{}.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetGlobal{}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(res.Changes); got["skills/a"] != harness.Added {
		t.Errorf("changes = %v", got)
	}
	if entries, _ := os.ReadDir(host.home); len(entries) > 0 {
		t.Errorf("a dry run wrote %v", entries)
	}
}

func TestDiffConfig_TellsAddedModifiedAndRemoved(t *testing.T) {
	host := newHost(t)
	a := fakeharness.Adapter{}
	writeHost(t, host, map[string]string{"settings.json": `{"theme":"light","editor":"vim"}`})
	applied := snapshot(
		component("settings", harness.KindJSONSubset, harness.Managed, map[string]string{"settings.json": `{"theme":"dark"}`}),
		component("skills/a", harness.KindDir, harness.Managed, map[string]string{"skills/a/SKILL.md": "a"}),
		component("skills/b", harness.KindDir, harness.Managed, map[string]string{"skills/b/SKILL.md": "b"}),
	)
	if _, err := a.ApplyConfig(t.Context(), host, applied, harness.ApplyOptions{Target: harness.TargetGlobal{}}); err != nil {
		t.Fatal(err)
	}
	d, err := a.DiffConfig(t.Context(), host, applied)
	if err != nil || len(d.Changes) != 0 {
		t.Fatalf("diff right after apply = %+v, %v", d, err)
	}

	writeHost(t, host, map[string]string{"skills/a/SKILL.md": "edited on the host", "settings.json": `{"theme":"dark","editor":"emacs"}`})
	next := snapshot(
		component("settings", harness.KindJSONSubset, harness.Managed, map[string]string{"settings.json": `{"theme":"solarized"}`}),
		component("skills/a", harness.KindDir, harness.Managed, map[string]string{"skills/a/SKILL.md": "a2"}),
		component("skills/c", harness.KindDir, harness.Managed, map[string]string{"skills/c/SKILL.md": "c"}),
	)
	d, err = a.DiffConfig(t.Context(), host, next)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]harness.ChangeKind{
		"settings": harness.Modified, // a foreign field changed, not the managed one
		"skills/a": harness.Modified, // edited on the host: drift is apply-drift-lock's
		"skills/b": harness.Removed,
		"skills/c": harness.Added,
	}
	if got := changes(d.Changes); !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
}

func TestApplyConfig_RefusesASnapshotItCannotWriteSafely(t *testing.T) {
	a := fakeharness.Adapter{Caps: allCaps}
	settings := func(path, content string) harness.Component {
		return component("settings", harness.KindJSONSubset, harness.Managed, map[string]string{path: content})
	}
	for name, snap := range map[string]harness.NativeSnapshot{
		"another harness":     {Harness: "claude", SchemaVersion: fakeharness.SchemaVersion},
		"another schema":      {Harness: "fake", SchemaVersion: 99, Components: snapshot(settings("settings.json", "{}")).Components},
		"unknown component":   snapshot(component("hooks", harness.KindFile, harness.Managed, map[string]string{"hooks": "x"})),
		"wrong kind":          snapshot(component("settings", harness.KindFile, harness.Managed, map[string]string{"settings.json": "{}"})),
		"wrong file":          snapshot(settings("other.json", "{}")),
		"settings not object": snapshot(settings("settings.json", "[1]")),
		"escape":              snapshot(component("skills/a", harness.KindDir, harness.Managed, map[string]string{"skills/a/../../x": "x"})),
		"outside its dir":     snapshot(component("skills/a", harness.KindDir, harness.Managed, map[string]string{"skills/b/x": "x"})),
		"the auth":            snapshot(component("skills/a", harness.KindDir, harness.Managed, map[string]string{"credentials.json": "x"})),
		"bad ownership":       snapshot(component("skills/a", harness.KindDir, "mine", nil)),
		"twice":               snapshot(settings("settings.json", "{}"), settings("settings.json", "{}")),
	} {
		for _, target := range []harness.Target{harness.TargetRun{Dir: t.TempDir()}, harness.TargetGlobal{}} {
			host := newHost(t)
			if _, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: target}); err == nil {
				t.Errorf("%s to %T: nil error", name, target)
			}
			if entries, _ := os.ReadDir(host.home); len(entries) > 0 {
				t.Errorf("%s to %T: wrote %v", name, target, entries)
			}
		}
	}
}

func TestValidateConfig_ReportsWhatTheProgramCannotRead(t *testing.T) {
	host := newHost(t)
	a := fakeharness.Adapter{}
	if v, err := a.ValidateConfig(t.Context(), host); err != nil || len(v.Issues) != 0 {
		t.Errorf("an empty configuration: %+v, %v", v, err)
	}
	writeHost(t, host, map[string]string{"settings.json": "not json", "fakeharness.json": `{"steps":[{}]}`, "skills/a/SKILL.md": "a"})
	v, err := a.ValidateConfig(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	var at []string
	for _, i := range v.Issues {
		at = append(at, i.Component)
	}
	if !slices.Equal(at, []string{"script", "settings"}) {
		t.Errorf("issues = %+v, want one for script and one for settings", v.Issues)
	}
}
