package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

// escMini is mini with markers in its TOML files.
type escMini struct{ *mini }

func (escMini) SecretEscape(p string) (harness.Escape, error) {
	if strings.HasSuffix(p, ".toml") {
		return harness.EscapeTOMLBasicString, nil
	}
	if strings.HasSuffix(p, ".json") {
		return harness.EscapeJSONString, nil
	}
	return harness.EscapeRaw, nil
}

const (
	secretA = "tok\"en\\with\nnewline-é-中-sentinel-AAAA" //nolint:gosec // a value of the test
	secretB = "second-sentinel-BBBB"
)

// withMarkers is a snapshot of mini with two TOML files that carry markers.
func withMarkers() harness.NativeSnapshot {
	file := func(p, content string, refs ...string) harness.File {
		return harness.File{Path: p, Mode: 0o644, Content: []byte(content), SecretRefs: refs}
	}
	return harness.NativeSnapshot{Harness: "mini", SchemaVersion: 1, Components: []harness.Component{
		{Name: "config", Kind: harness.KindFile, Ownership: harness.Managed, Files: []harness.File{
			file("config.toml", "model = \"x\"\ntoken = \""+harness.SecretMarker("team/a")+"\"\n", "team/a"),
		}},
		{Name: "extra", Kind: harness.KindDir, Ownership: harness.Managed, Files: []harness.File{
			file("extra/more.toml", "a = \""+harness.SecretMarker("team/a")+"\"\nb = \""+harness.SecretMarker("team/b")+"\"\n", "team/a", "team/b"),
			file("extra/plain.txt", "no secrets here\n"),
		}},
	}}
}

var values = map[string][]byte{"team/a": []byte(secretA), "team/b": []byte(secretB)}

func allowAll() error { return nil }

// registryFence is the fence of the executor over the real secretjit.
func registryFence(runDir string, attempt uint64) Fence {
	return func(paths []string, write func() error) error {
		return secretjit.New().Render(runDir, attempt, paths, allowAll, write)
	}
}

// crashFence registers like registryFence but, as a process that dies would,
// does not clean up when write fails: the error is handed back apart.
func crashFence(runDir string, attempt uint64, died *error) Fence {
	return func(paths []string, write func() error) error {
		return secretjit.New().Render(runDir, attempt, paths, allowAll, func() error {
			*died = write()
			return nil
		})
	}
}

func newEscFixture(t *testing.T) *fixture {
	f := newFixture(t)
	return f
}

func (f *fixture) materializeEsc(snap harness.NativeSnapshot) (Materialization, error) {
	return f.m.Materialize(context.Background(), escMini{f.a}, f.runDir, snap)
}

func TestTOMLBasicStringReadsBackWhatWasEscaped(t *testing.T) {
	for _, v := range []string{
		`plain`, `with "quotes"`, `back\slash`, `a\"b\\`, "line\nbreak", "tab\tcr\rbell\x07", "\x00\x01\x1f\x7f",
		"unicode é 中 😀", `A \n not an escape`, `{{cf-secret:team/a}}`, "'single' and # hash", `a = "b"`, "  ",
	} {
		out, err := escapeValue(harness.EscapeTOMLBasicString, []byte(v))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		var parsed struct{ V string }
		if err := toml.Unmarshal([]byte(`v = "`+string(out)+`"`), &parsed); err != nil || parsed.V != v {
			t.Errorf("%q escaped as %q reads back %q (%v)", v, out, parsed.V, err)
		}
	}
	if _, err := escapeValue(harness.EscapeTOMLBasicString, []byte{0xff, 0xfe}); err == nil {
		t.Error("a value that is not UTF-8 was escaped")
	}
}

func TestRenderWritesTheEscapedValuesPrivatelyAndAtomically(t *testing.T) {
	f := newEscFixture(t)
	mat, err := f.materializeEsc(withMarkers())
	if err != nil {
		t.Fatal(err)
	}
	if len(mat.Rendered) != 2 || mat.Rendered[0].Path != "config.toml" || mat.Rendered[1].Escape != harness.EscapeTOMLBasicString {
		t.Fatalf("Rendered = %+v", mat.Rendered)
	}
	// Unrendered, the tree is what the record digests.
	f.assertDone(mat.Digest)

	if err := f.m.Render(f.runDir, mat, values, registryFence(f.runDir, 1)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ file, key, want string }{
		{"config.toml", "token", secretA}, {"extra/more.toml", "a", secretA}, {"extra/more.toml", "b", secretB},
	} {
		raw, err := os.ReadFile(filepath.Join(f.path("config"), filepath.FromSlash(c.file))) //nolint:gosec // a test tree
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := toml.Unmarshal(raw, &parsed); err != nil || parsed[c.key] != c.want {
			t.Errorf("%s %s = %v (%v)", c.file, c.key, parsed[c.key], err)
		}
		if info, _ := os.Stat(filepath.Join(f.path("config"), filepath.FromSlash(c.file))); info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", c.file, info.Mode())
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(f.path("config"), "extra", "plain.txt")); string(raw) != "no secrets here\n" { //nolint:gosec // a test tree
		t.Errorf("a file without markers changed: %q", raw)
	}
	entries, _ := os.ReadDir(f.path("config"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".cf-render") {
			t.Errorf("temporary left: %s", e.Name())
		}
	}
	if !secretjit.HasRendered(f.runDir) {
		t.Error("the rendered files are not registered")
	}
}

func TestRegistrationComesBeforeAnyValue(t *testing.T) {
	f := newEscFixture(t)
	mat, err := f.materializeEsc(withMarkers())
	if err != nil {
		t.Fatal(err)
	}
	fence := func(paths []string, write func() error) error {
		return registryFence(f.runDir, 1)(paths, func() error {
			if !secretjit.HasRendered(f.runDir) {
				t.Error("write ran before the registration")
			}
			noRegularFileContains(t, f.runDir, "sentinel")
			return write()
		})
	}
	if err := f.m.Render(f.runDir, mat, values, fence); err != nil {
		t.Fatal(err)
	}
}

func TestRenderNeedsEveryValueAndTouchesNothingWithout(t *testing.T) {
	f := newEscFixture(t)
	mat, _ := f.materializeEsc(withMarkers())
	before := manifest(t, f.runDir)
	err := f.m.Render(f.runDir, mat, map[string][]byte{"team/a": []byte(secretA)}, registryFence(f.runDir, 1))
	if err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("err = %v", err)
	}
	sameManifest(t, "run dir", before, manifest(t, f.runDir))
}

func TestMaterializeRefusesMarkersWhoseAdapterNamesNoEscape(t *testing.T) {
	f := newFixture(t)
	_, err := f.materialize(withMarkers()) // mini says no escape
	if !errors.Is(err, harness.ErrSecretUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if f.a.applies.Load() != 0 {
		t.Error("ApplyConfig was called")
	}
	if entries, _ := os.ReadDir(f.runDir); len(entries) != 0 {
		t.Errorf("run dir holds %v", entries)
	}
}

// A render that fails at each of its durability boundaries, or a process that
// dies there, leaves nothing the release or the next prepare cannot remove,
// and the next prepare rebuilds a tree with the new value and no old one.
func TestRenderCrashedAtEachBoundaryLeavesNoOrphanValue(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name   string
		inject func(f *fixture)
	}{
		{"temporary synced", func(f *fixture) {
			sync := f.m.fs.syncIn
			f.m.fs.syncIn = func(r *os.Root, p string) error {
				if strings.HasSuffix(p, ".more.toml.cf-render") {
					return boom
				}
				return sync(r, p)
			}
		}},
		{"before the rename of the second file", func(f *fixture) {
			rename := f.m.fs.renameIn
			f.m.fs.renameIn = func(r *os.Root, from, to string) error {
				if strings.HasSuffix(to, "more.toml") {
					return boom
				}
				return rename(r, from, to)
			}
		}},
		{"directory synced after a rename", func(f *fixture) {
			sync := f.m.fs.syncIn
			f.m.fs.syncIn = func(r *os.Root, p string) error {
				if p == "config" {
					return boom
				}
				return sync(r, p)
			}
		}},
	}
	for _, c := range cases {
		for _, mode := range []string{"release", "rebuild"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				f := newEscFixture(t)
				mat, err := f.materializeEsc(withMarkers())
				if err != nil {
					t.Fatal(err)
				}
				fs := f.m.fs
				c.inject(f)
				var died error
				if err := f.m.Render(f.runDir, mat, values, crashFence(f.runDir, 1, &died)); err != nil || !errors.Is(died, boom) {
					t.Fatalf("render = %v, died = %v", err, died)
				}
				f.m.fs = fs
				if !holds(f.runDir, "sentinel") {
					t.Fatal("the crash left no value on disk: the case proves nothing")
				}
				switch mode {
				case "release":
					if err := secretjit.New().Release(f.runDir, allowAll); err != nil {
						t.Fatal(err)
					}
					noRegularFileContains(t, f.runDir, "sentinel")
					if secretjit.HasRendered(f.runDir) == false {
						t.Error("the registry is gone: the Run forgot that it used secrets")
					}
				case "rebuild":
					mat2, err := f.materializeEsc(withMarkers())
					if err != nil {
						t.Fatal(err)
					}
					rotated := map[string][]byte{"team/a": []byte("rotated-AAAA-sentinel"), "team/b": []byte(secretB)}
					if err := f.m.Render(f.runDir, mat2, rotated, registryFence(f.runDir, 2)); err != nil {
						t.Fatal(err)
					}
					noRegularFileContains(t, f.runDir, "tok\\\"en")
					raw, _ := os.ReadFile(filepath.Join(f.path("config"), "config.toml")) //nolint:gosec // a test tree
					if !strings.Contains(string(raw), "rotated-AAAA-sentinel") {
						t.Errorf("config.toml = %q", raw)
					}
					entries, _ := os.ReadDir(f.path("config"))
					for _, e := range entries {
						if strings.HasSuffix(e.Name(), ".cf-render") {
							t.Errorf("temporary left: %s", path.Join("config", e.Name()))
						}
					}
					if err := secretjit.New().Release(f.runDir, allowAll); err != nil {
						t.Fatal(err)
					}
					noRegularFileContains(t, f.runDir, "sentinel")
				}
			})
		}
	}
}

// Rendered, a tree no longer digests to its record: Materialize rebuilds it,
// which removes every value it held.
func TestMaterializeRebuildsARenderedTree(t *testing.T) {
	f := newEscFixture(t)
	mat, _ := f.materializeEsc(withMarkers())
	if err := f.m.Render(f.runDir, mat, values, registryFence(f.runDir, 1)); err != nil {
		t.Fatal(err)
	}
	f.a.applies.Store(0)
	again, err := f.materializeEsc(withMarkers())
	if err != nil {
		t.Fatal(err)
	}
	if f.a.applies.Load() != 1 || again.Digest != mat.Digest {
		t.Errorf("applies = %d, digest %s, want a rebuild to %s", f.a.applies.Load(), again.Digest, mat.Digest)
	}
	noRegularFileContains(t, f.runDir, "sentinel")
	// Unrendered, it is reused.
	f.a.applies.Store(0)
	if _, err := f.materializeEsc(withMarkers()); err != nil || f.a.applies.Load() != 0 {
		t.Errorf("an unrendered tree was not reused: %d applies (%v)", f.a.applies.Load(), err)
	}
}

// holds says whether a regular file under root holds s.
func holds(root, s string) bool {
	found := false
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), s) { //nolint:gosec // a test tree
				found = true
			}
		}
		return nil
	})
	return found
}

// A directory of the tree swapped for a symlink between the write of the
// temporary and its rename does not take the rendered file out of the run
// directory.
func TestRenderDoesNotFollowASymlinkSwappedInBeforeTheRename(t *testing.T) {
	f := newEscFixture(t)
	mat, err := f.materializeEsc(withMarkers())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sync := f.m.fs.syncIn
	f.m.fs.syncIn = func(r *os.Root, p string) error {
		if err := sync(r, p); err != nil || !strings.HasSuffix(p, ".more.toml.cf-render") {
			return err
		}
		data, err := r.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outside, path.Base(p)), data, 0o600); err != nil { //nolint:gosec // a temporary directory
			return err
		}
		if err := os.Rename(f.path("config"), f.path("config.moved")); err != nil {
			return err
		}
		return os.Symlink(outside, f.path("config"))
	}
	var died error
	if err := f.m.Render(f.runDir, mat, values, crashFence(f.runDir, 1, &died)); err != nil || died == nil {
		t.Fatalf("render = %v, died = %v", err, died)
	}
	if _, err := os.Lstat(filepath.Join(outside, "more.toml")); err == nil {
		t.Error("the rendered file was moved outside the run directory")
	}
}

func TestJSONStringReadsBackWhatWasEscaped(t *testing.T) {
	for _, v := range []string{
		`plain`, `with "quotes"`, `back\slash`, `a\"b\\`, "line\nbreak", "tab\tcr\rbell\x07", "\x00\x01\x1f\x7f",
		"unicode é 中 😀", "sep \u2028 \u2029", `A \n not an escape`, `{{cf-secret:team/a}}`, "<a href=\"x\">&</a>", "",
	} {
		out, err := escapeValue(harness.EscapeJSONString, []byte(v))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		var parsed struct{ V string }
		if err := json.Unmarshal([]byte(`{"v":"`+string(out)+`"}`), &parsed); err != nil || parsed.V != v {
			t.Errorf("%q escaped as %q reads back %q (%v)", v, out, parsed.V, err)
		}
	}
	if _, err := escapeValue(harness.EscapeJSONString, []byte{0xff, 0xfe}); err == nil {
		t.Error("a value that is not UTF-8 was escaped")
	}
}

func TestRenderOfAJSONFileParsesBackToTheValues(t *testing.T) {
	f := newEscFixture(t)
	snap := harness.NativeSnapshot{Harness: "mini", SchemaVersion: 1, Components: []harness.Component{{
		Name: "settings", Kind: harness.KindFile, Ownership: harness.Managed, Files: []harness.File{{
			Path: "settings.json", Mode: 0o644, SecretRefs: []string{"team/a", "team/b"},
			Content: []byte(`{"env": {"A": "` + harness.SecretMarker("team/a") + `"}, "list": ["x-` + harness.SecretMarker("team/b") + `-y"]}`),
		}},
	}}}
	mat, err := f.materializeEsc(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(mat.Rendered) != 1 || mat.Rendered[0].Escape != harness.EscapeJSONString {
		t.Fatalf("Rendered = %+v", mat.Rendered)
	}
	if err := f.m.Render(f.runDir, mat, values, registryFence(f.runDir, 1)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.path("config"), "settings.json")) //nolint:gosec // a test tree
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Env  map[string]string
		List []string
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the rendered file is not JSON: %v\n%s", err, raw)
	}
	if parsed.Env["A"] != secretA || len(parsed.List) != 1 || parsed.List[0] != "x-"+secretB+"-y" {
		t.Errorf("rendered = %+v, want the values back", parsed)
	}
}
