package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// TestScenario_HarnessAdapterFramework_Aislamiento: two Runs with different
// configurations each get their own, materialised beside them; HOME is never
// touched and the auth of the host is linked, out of the digest.
func TestScenario_HarnessAdapterFramework_Aislamiento(t *testing.T) {
	ctx := context.Background()
	h, _ := testHost(t)
	a := newMini()
	m := NewMaterializer(h)
	runs := t.TempDir()
	runA, runB := filepath.Join(runs, "a-e1"), filepath.Join(runs, "b-e1")
	for _, d := range []string{runA, runB} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	homeBefore := manifest(t, h.Home())

	matA, err := m.Materialize(ctx, a, runA, snapshot("A"))
	if err != nil {
		t.Fatalf("Materialize A: %v", err)
	}
	matB, err := m.Materialize(ctx, a, runB, snapshot("B"))
	if err != nil {
		t.Fatalf("Materialize B: %v", err)
	}

	// Each Run has its own config/ and config.json, with different digests.
	for _, c := range []struct {
		run string
		mat Materialization
	}{{runA, matA}, {runB, matB}} {
		if c.mat.ConfigDir != filepath.Join(c.run, "config") {
			t.Errorf("ConfigDir = %q, want under %s", c.mat.ConfigDir, c.run)
		}
		info, err := os.Stat(c.mat.ConfigDir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want a 0700 directory", c.mat.ConfigDir, info, err)
		}
		var rec Materialization
		data, err := os.ReadFile(filepath.Join(c.run, "config.json")) //nolint:gosec // a test path
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Digest != c.mat.Digest || rec.SourceDigest != c.mat.SourceDigest || rec.Harness != "mini" || rec.Schema != 1 {
			t.Errorf("config.json of %s = %+v, want %+v", c.run, rec, c.mat)
		}
		if got, err := treedigest.Dir(c.mat.ConfigDir, treedigest.Options{ApprovedLinks: []string{"credentials.json"}}); err != nil || got != c.mat.Digest {
			t.Errorf("digest of %s = %s, %v; recorded %s", c.mat.ConfigDir, got, err, c.mat.Digest)
		}
	}
	if matA.Digest == matB.Digest || matA.SourceDigest == matB.SourceDigest {
		t.Fatalf("A and B share a digest: %+v / %+v", matA, matB)
	}

	// The digest is the configuration alone: the files of the snapshot.
	if want := snapshotDigest(t, snapshot("A")); matA.Digest != want {
		t.Errorf("digest of A = %s, want that of its snapshot, %s", matA.Digest, want)
	}

	// Materialising A again gives the same digest and leaves B alone.
	bBefore := manifest(t, runB)
	again, err := m.Materialize(ctx, a, runA, snapshot("A"))
	if err != nil || again.Digest != matA.Digest {
		t.Errorf("Materialize A again = %s, %v; want %s", again.Digest, err, matA.Digest)
	}
	runC := filepath.Join(runs, "c-e1")
	if err := os.Mkdir(runC, 0o700); err != nil {
		t.Fatal(err)
	}
	if matC, err := m.Materialize(ctx, a, runC, snapshot("A")); err != nil || matC.Digest != matA.Digest {
		t.Errorf("A in another Run = %s, %v; want %s", matC.Digest, err, matA.Digest)
	}
	sameManifest(t, "run B", bBefore, manifest(t, runB))

	// HOME is unchanged, symlinks and permissions included.
	sameManifest(t, "HOME", homeBefore, manifest(t, h.Home()))

	// The credentials are linked, never copied, and outside the digest.
	cred := filepath.Join(h.Home(), ".mini", "credentials.json")
	for _, mat := range []Materialization{matA, matB} {
		link := filepath.Join(mat.ConfigDir, "credentials.json")
		if target, err := os.Readlink(link); err != nil || target != cred {
			t.Errorf("%s -> %q, %v; want a symlink to %s", link, target, err, cred)
		}
		noRegularFileContains(t, filepath.Dir(mat.ConfigDir), credentialSentinel)
	}
	writeFile(t, cred, "rotated-"+credentialSentinel, 0o600)
	if got, err := treedigest.Dir(matA.ConfigDir, treedigest.Options{ApprovedLinks: matA.AuthLinks}); err != nil || got != matA.Digest {
		t.Errorf("after rotating the credentials the digest is %s, %v; want %s", got, err, matA.Digest)
	}
}

func snapshotDigest(t *testing.T, snap harness.NativeSnapshot) string {
	t.Helper()
	var files []harness.File
	for _, c := range snap.Components {
		files = append(files, c.Files...)
	}
	d, err := harness.FilesDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fixture is a run directory and a Materializer for it.
type fixture struct {
	t      *testing.T
	a      *mini
	m      *Materializer
	runDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	h, _ := testHost(t)
	runDir := filepath.Join(t.TempDir(), "run-e1")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, a: newMini(), m: NewMaterializer(h), runDir: runDir}
}

func (f *fixture) materialize(snap harness.NativeSnapshot) (Materialization, error) {
	return f.m.Materialize(context.Background(), f.a, f.runDir, snap)
}

func (f *fixture) must(snap harness.NativeSnapshot) Materialization {
	f.t.Helper()
	mat, err := f.materialize(snap)
	if err != nil {
		f.t.Fatalf("Materialize: %v", err)
	}
	return mat
}

func (f *fixture) path(name string) string { return filepath.Join(f.runDir, name) }

// assertDone fails unless the run directory holds exactly a materialisation
// with digest: config/ and config.json, no staging.
func (f *fixture) assertDone(digest string) {
	f.t.Helper()
	if !exists(f.t, f.path("config")) || !exists(f.t, f.path("config.json")) || exists(f.t, f.path("config.staging")) {
		f.t.Errorf("run dir: config %v, config.json %v, config.staging %v; want config and config.json alone",
			exists(f.t, f.path("config")), exists(f.t, f.path("config.json")), exists(f.t, f.path("config.staging")))
	}
	if got, err := treedigest.Dir(f.path("config"), treedigest.Options{ApprovedLinks: []string{"credentials.json"}}); err != nil || got != digest {
		f.t.Errorf("config digests to %s, %v; want %s", got, err, digest)
	}
	if info, err := os.Lstat(filepath.Join(f.path("config"), "credentials.json")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		f.t.Errorf("the auth link is not there: %v", err)
	}
}

// assertNotDone fails if the run directory holds a config.json: nothing left
// must pass for a finished materialisation.
func (f *fixture) assertNotDone() {
	f.t.Helper()
	if exists(f.t, f.path("config.json")) {
		f.t.Error("config.json exists after a failure")
	}
}

// TestMaterialize_StateOnDisk walks the table of what a materialisation may
// find in the run directory.
func TestMaterialize_StateOnDisk(t *testing.T) {
	type setup func(f *fixture)
	writeRecord := func(edit func(rec map[string]any)) setup {
		return func(f *fixture) {
			data, err := os.ReadFile(f.path("config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var rec map[string]any
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatal(err)
			}
			edit(rec)
			data, err = json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.path("config.json"), string(data), 0o600)
		}
	}
	cases := []struct {
		name string
		// before is set up after a first materialisation; nil starts empty.
		before  setup
		applies int32
	}{
		{name: "nothing", applies: 1},
		{name: "only staging", applies: 1, before: func(f *fixture) {
			if err := os.Remove(f.path("config.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(f.path("config"), f.path("config.staging")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "config without config.json", applies: 1, before: func(f *fixture) {
			if err := os.Remove(f.path("config.json")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(f.path("config"), "leftover"), "x", 0o600)
		}},
		{name: "config.json without config", applies: 1, before: func(f *fixture) {
			if err := os.RemoveAll(f.path("config")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "config.json corrupt", applies: 1, before: func(f *fixture) {
			writeFile(t, f.path("config.json"), `{"schema":1,"harn`, 0o600)
		}},
		{name: "config.json of another harness", applies: 1, before: writeRecord(func(r map[string]any) { r["harness"] = "other" })},
		{name: "config.json of another snapshot", applies: 1, before: writeRecord(func(r map[string]any) { r["source_digest"] = "sha256:00" })},
		{name: "config.json of another schema", applies: 1, before: writeRecord(func(r map[string]any) { r["schema"] = 2 })},
		{name: "config edited since", applies: 1, before: func(f *fixture) {
			writeFile(t, filepath.Join(f.path("config"), "settings.json"), `{"theme":"edited"}`, 0o600)
		}},
		{name: "an auth link removed", applies: 1, before: func(f *fixture) {
			if err := os.Remove(filepath.Join(f.path("config"), "credentials.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "both coherent", applies: 0},
		{name: "both coherent with a stray staging", applies: 0, before: func(f *fixture) {
			if err := os.Mkdir(f.path("config.staging"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			var want Materialization
			if c.name != "nothing" {
				want = f.must(snapshot("A"))
				if c.before != nil {
					c.before(f)
				}
				f.a.applies.Store(0)
			}
			got := f.must(snapshot("A"))
			if n := f.a.applies.Load(); n != c.applies {
				t.Errorf("ApplyConfig called %d times, want %d", n, c.applies)
			}
			if want.Digest != "" && got.Digest != want.Digest {
				t.Errorf("digest = %s, want %s", got.Digest, want.Digest)
			}
			f.assertDone(got.Digest)
			if c.name == "both coherent" && !got.At.Equal(want.At) {
				t.Errorf("a reused materialisation has a new time: %v, was %v", got.At, want.At)
			}
		})
	}
}

// TestMaterialize_FailsAtEachBoundaryAndRecovers injects a failure at every
// step that makes a materialisation durable: only one after config.json is in
// place leaves it, and the next Materialize finishes the job.
func TestMaterialize_FailsAtEachBoundaryAndRecovers(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name   string
		inject func(f *fixture)
		// leavesConfig: the failure comes after config/ is published.
		leavesConfig bool
		// leavesRecord: the failure comes after config.json is published, so
		// the next Materialize finds both coherent and reuses them.
		leavesRecord bool
	}{
		{name: "apply config", inject: func(f *fixture) { f.a.applyErr = boom }},
		{name: "fsync of staging", inject: func(f *fixture) {
			sync := f.m.fs.sync
			f.m.fs.sync = func(p string) error {
				if strings.HasSuffix(p, filepath.Join("config.staging", "skills", "run.sh")) {
					return boom
				}
				return sync(p)
			}
		}},
		{name: "rename", inject: func(f *fixture) { f.m.fs.rename = func(string, string) error { return boom } }},
		{name: "fsync of the parent", leavesConfig: true, inject: func(f *fixture) {
			sync := f.m.fs.sync
			f.m.fs.sync = func(p string) error {
				if p == f.runDir && exists(f.t, f.path("config")) {
					return boom
				}
				return sync(p)
			}
		}},
		{name: "write config.json", leavesConfig: true, inject: func(f *fixture) {
			f.m.fs.writeRecord = func(string, []byte) error { return boom }
		}},
		{name: "fsync of the parent after config.json", leavesConfig: true, leavesRecord: true, inject: func(f *fixture) {
			// config.json is renamed into place; syncing the run directory fails.
			f.m.fs.writeRecord = func(path string, data []byte) error {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					return err
				}
				return boom
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			fs := f.m.fs
			c.inject(f)
			if _, err := f.materialize(snapshot("A")); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
			if got := exists(t, f.path("config.json")); got != c.leavesRecord {
				t.Errorf("config.json exists: %v, want %v", got, c.leavesRecord)
			}
			if exists(t, f.path("config.staging")) {
				t.Error("config.staging left behind")
			}
			if got := exists(t, f.path("config")); got != c.leavesConfig {
				t.Errorf("config exists: %v, want %v", got, c.leavesConfig)
			}

			f.a.applyErr, f.m.fs = nil, fs
			f.a.applies.Store(0)
			mat := f.must(snapshot("A"))
			want := int32(1)
			if c.leavesRecord {
				want = 0
			}
			if f.a.applies.Load() != want {
				t.Errorf("recovery applied the configuration %d times, want %d", f.a.applies.Load(), want)
			}
			f.assertDone(mat.Digest)
			if want := snapshotDigest(t, snapshot("A")); mat.Digest != want {
				t.Errorf("recovered digest %s, want %s", mat.Digest, want)
			}
		})
	}
}

func TestMaterialize_RefusesWithoutTouchingTheRunDir(t *testing.T) {
	withSecret := snapshot("A")
	withSecret.Components[0].Files[0].Content = []byte(`{"token":"` + harness.SecretMarker("github/token") + `"}`)
	malformedMarker := snapshot("A")
	malformedMarker.Components[0].Files[0].Content = []byte(`{"token":"{{cf-secret:../github/token}}"}`)
	declaredSecret := snapshot("A")
	declaredSecret.Components[0].Files[0].SecretRefs = []string{"github/token"}
	otherHarness := snapshot("A")
	otherHarness.Harness = "other"
	badDigest := snapshot("A")
	badDigest.Components[0].Digest = "sha256:00"
	twice := snapshot("A")
	twice.Components[1].Name = "settings"

	cases := []struct {
		name string
		snap harness.NativeSnapshot
		is   error
	}{
		{"secret marker in content", withSecret, harness.ErrSecretUnavailable},
		{"secret marker that is not valid", malformedMarker, harness.ErrSecretUnavailable},
		{"secret refs declared", declaredSecret, harness.ErrSecretUnavailable},
		{"snapshot of another harness", otherHarness, nil},
		{"component digest that is not its files'", badDigest, nil},
		{"component twice", twice, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.materialize(c.snap)
			if err == nil || (c.is != nil && !errors.Is(err, c.is)) {
				t.Fatalf("err = %v, want %v", err, c.is)
			}
			if f.a.applies.Load() != 0 {
				t.Error("ApplyConfig was called")
			}
			if entries, _ := os.ReadDir(f.runDir); len(entries) != 0 {
				t.Errorf("run dir holds %v", entries)
			}
		})
	}
}

func TestMaterialize_RefusesASymlinkTheAdapterDidNotReport(t *testing.T) {
	f := newFixture(t)
	f.a.strayLink = true
	if _, err := f.materialize(snapshot("A")); !errors.Is(err, treedigest.ErrUnsupportedType) {
		t.Fatalf("err = %v, want ErrUnsupportedType", err)
	}
	f.assertNotDone()
	if exists(t, f.path("config")) || exists(t, f.path("config.staging")) {
		t.Error("a configuration was left behind")
	}
}

func TestMaterialize_RefusesAnAdapterWithoutConfigIsolation(t *testing.T) {
	f := newFixture(t)
	f.a.caps.ConfigIsolation = false
	if _, err := f.materialize(snapshot("A")); !errors.Is(err, harness.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestMaterialize_EmptySnapshotIsAnEmptyConfiguration(t *testing.T) {
	f := newFixture(t)
	mat := f.must(harness.NativeSnapshot{})
	empty, err := treedigest.Files(nil)
	if err != nil {
		t.Fatal(err)
	}
	if mat.Digest != empty || len(mat.Components) != 0 || mat.Harness != "mini" {
		t.Errorf("empty snapshot = %+v, want the empty digest %s", mat, empty)
	}
	f.assertDone(empty)
}

func TestMaterialize_RecordsTheComponentsAndTheTime(t *testing.T) {
	f := newFixture(t)
	mat := f.must(snapshot("A"))
	if len(mat.Components) != 2 || mat.Components[0].Name != "settings" || mat.Components[1].Name != "skills" {
		t.Fatalf("components = %+v", mat.Components)
	}
	for i, c := range snapshot("A").Components {
		if want := snapshotDigest(t, harness.NativeSnapshot{Components: []harness.Component{c}}); mat.Components[i].Digest != want {
			t.Errorf("component %s digest %s, want %s", c.Name, mat.Components[i].Digest, want)
		}
	}
	if got := f.m.host.Clock().Now(); !mat.At.Equal(got) {
		t.Errorf("materialized_at = %v, want the clock of the host, %v", mat.At, got)
	}
	if len(mat.AuthLinks) != 1 || mat.AuthLinks[0] != "credentials.json" {
		t.Errorf("auth links = %v", mat.AuthLinks)
	}
}

func TestMaterialize_AuthCopyIsLeftOutOfTheDigestAndReused(t *testing.T) {
	f := newFixture(t)
	f.a.copyAuth = true
	mat := f.must(snapshot("A"))
	if len(mat.AuthCopies) != 1 || mat.AuthCopies[0] != "credentials.json" || len(mat.AuthLinks) != 0 {
		t.Fatalf("auth copies = %v, links = %v", mat.AuthCopies, mat.AuthLinks)
	}
	if want := []string{filepath.Join(f.path("config"), "credentials.json")}; !slices.Equal(mat.AuthCopyPaths(), want) {
		t.Errorf("AuthCopyPaths = %v, want %v", mat.AuthCopyPaths(), want)
	}
	if got, err := treedigest.Dir(f.path("config"), treedigest.Options{ApprovedFiles: mat.AuthCopies}); err != nil || got != mat.Digest {
		t.Errorf("config digests to %s, %v; want %s", got, err, mat.Digest)
	}
	read, err := ReadMaterialization(f.runDir)
	if err != nil || !slices.Equal(read.AuthCopies, mat.AuthCopies) || read.ConfigDir != mat.ConfigDir {
		t.Errorf("ReadMaterialization = %+v, %v", read, err)
	}
	// The harness rewrites its copy: that is no reason to rebuild.
	if err := os.WriteFile(filepath.Join(f.path("config"), "credentials.json"), []byte(`{"refreshed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	applies := f.a.applies.Load()
	again := f.must(snapshot("A"))
	if f.a.applies.Load() != applies || again.Digest != mat.Digest {
		t.Error("a rewritten auth copy made the materialisation be rebuilt")
	}
	// A copy that is gone is.
	if err := os.Remove(filepath.Join(f.path("config"), "credentials.json")); err != nil {
		t.Fatal(err)
	}
	f.must(snapshot("A"))
	if f.a.applies.Load() == applies {
		t.Error("a missing auth copy was reused")
	}
}

// A crash between the copy of the auth and config.json leaves the copy in the
// run directory with no record that names it: the janitor and forget of the
// executor remove it by what the layout is (see executor.removeMaterialization).
// This pins the layouts they depend on, at each point of the build.
func TestMaterialize_AuthCopyIsOnDiskBeforeItsRecord(t *testing.T) {
	cases := []struct {
		name   string
		arm    func(f *fixture, crash func())
		copyAt string
	}{
		{name: "after the copy, in staging", copyAt: "config.staging", arm: func(f *fixture, crash func()) {
			sync := f.m.fs.sync
			f.m.fs.sync = func(p string) error {
				if strings.HasSuffix(p, filepath.Join("config.staging", "credentials.json")) {
					crash()
				}
				return sync(p)
			}
		}},
		{name: "after the rename, before config.json", copyAt: "config", arm: func(f *fixture, crash func()) {
			sync := f.m.fs.sync
			f.m.fs.sync = func(p string) error {
				if p == f.runDir && exists(f.t, f.path("config")) {
					crash()
				}
				return sync(p)
			}
		}},
		{name: "while config.json is written", copyAt: "config", arm: func(f *fixture, crash func()) {
			write := f.m.fs.writeRecord
			f.m.fs.writeRecord = func(p string, data []byte) error {
				crash()
				return write(p, data)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.a.copyAuth = true
			crashed := false
			c.arm(f, func() {
				crashed = true
				if !exists(t, filepath.Join(f.path(c.copyAt), "credentials.json")) {
					t.Errorf("no auth copy in %s at the crash", c.copyAt)
				}
				if exists(t, f.path("config.json")) {
					t.Error("config.json exists at the crash: nothing names the copy")
				}
			})
			f.must(snapshot("A"))
			if !crashed {
				t.Fatal("the build never reached the crash point")
			}
		})
	}
	if got := StagingDir("/r"); got != filepath.Join("/r", "config.staging") {
		t.Errorf("StagingDir = %s", got)
	}
}
