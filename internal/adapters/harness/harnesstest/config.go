package harnesstest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// snapshot is the configuration of the home of e, and false when the
// adapter does not snapshot. Then every operation on a snapshot must be
// unsupported too.
func (e *env) snapshot(t *testing.T) (harness.NativeSnapshot, bool) {
	t.Helper()
	var s harness.NativeSnapshot
	var err error
	e.homeUnchanged(t, "SnapshotConfig", func() { s, err = e.a.SnapshotConfig(t.Context(), e.host, harness.SnapshotOptions{}) })
	if errors.Is(err, harness.ErrUnsupported) {
		isUnsupported(t, "SnapshotConfig", s, err)
		if e.caps.ConfigIsolation {
			t.Error("ConfigIsolation is declared but SnapshotConfig is unsupported")
		}
		empty := harness.NativeSnapshot{Harness: e.a.ID()}
		res, err := e.a.ApplyConfig(t.Context(), e.host, empty, harness.ApplyOptions{Target: harness.TargetGlobal{}, DryRun: true})
		isUnsupported(t, "ApplyConfig with no SnapshotConfig", res, err)
		diff, err := e.a.DiffConfig(t.Context(), e.host, empty)
		isUnsupported(t, "DiffConfig with no SnapshotConfig", diff, err)
		return s, false
	}
	if err != nil {
		t.Fatalf("SnapshotConfig: %v", err)
	}
	return s, true
}

// rotateCredentials changes the value of every secret in the credential
// files, as a refreshed token would.
func (e *env) rotateCredentials(t *testing.T) {
	t.Helper()
	for _, rel := range e.f.CredentialFiles {
		p := filepath.Join(e.home, rel)
		b, err := os.ReadFile(p) //nolint:gosec // under the home of the check
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range e.f.Secrets {
			b = bytes.ReplaceAll(b, []byte(s), []byte(s+"-rotated"))
		}
		if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // under the home of the check
			t.Fatal(err)
		}
	}
}

func checkSnapshot(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	s, ok := e.snapshot(t)
	if !ok {
		return
	}
	if again, _ := e.snapshot(t); !reflect.DeepEqual(again, s) {
		t.Errorf("SnapshotConfig is not deterministic:\n%s\nthen\n%s", snapshotString(s), snapshotString(again))
	}
	if s.Harness != e.a.ID() {
		t.Errorf("snapshot Harness = %q, want %q", s.Harness, e.a.ID())
	}
	credentials := e.credentialContents(t)
	refs := 0
	for i, c := range s.Components {
		if c.Name == "" || slices.ContainsFunc(s.Components[:i], func(o harness.Component) bool { return o.Name == c.Name }) {
			t.Errorf("component %d has an empty or repeated name %q", i, c.Name)
		}
		if !slices.Contains([]harness.ComponentKind{harness.KindFile, harness.KindDir, harness.KindJSONSubset}, c.Kind) {
			t.Errorf("component %s has kind %q", c.Name, c.Kind)
		}
		if c.Ownership != harness.Managed && c.Ownership != harness.Local {
			t.Errorf("component %s has ownership %q", c.Name, c.Ownership)
		}
		if want := filesDigest(t, c.Files); c.Digest != want {
			t.Errorf("component %s: Digest %s, want the treedigest of its files %s", c.Name, c.Digest, want)
		}
		for _, file := range c.Files {
			if secret, leaked := e.leaks(append([]byte(file.Path+"\n"), file.Content...)); leaked {
				t.Errorf("component %s file %s carries the secret %q", c.Name, file.Path, secret)
			}
			for rel, content := range credentials {
				if bytes.Contains(file.Content, content) {
					t.Errorf("component %s file %s carries the credential file %s", c.Name, file.Path, rel)
				}
			}
			if err := file.CheckSecrets(); err != nil {
				t.Errorf("component %s: %v", c.Name, err)
			}
			refs += len(file.SecretRefs)
		}
	}
	if refs == 0 {
		t.Error("the snapshot has no secret ref, but the home has a {{cf-secret:<ref>}} marker: it was dropped or rendered")
	}
	e.rotateCredentials(t)
	if rotated, _ := e.snapshot(t); !reflect.DeepEqual(rotated, s) {
		t.Errorf("rotating the credentials changed the snapshot:\n%s\nthen\n%s", snapshotString(s), snapshotString(rotated))
	}
}

// snapshotString is s with each file as its path, mode and content.
func snapshotString(s harness.NativeSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s schema %d\n", s.Harness, s.SchemaVersion)
	for _, c := range s.Components {
		fmt.Fprintf(&b, "  %s (%s, %s) %s\n", c.Name, c.Kind, c.Ownership, c.Digest)
		for _, f := range c.Files {
			fmt.Fprintf(&b, "    %s %s refs=%q %q\n", f.Path, f.Mode, f.SecretRefs, f.Content)
		}
	}
	return b.String()
}

func (e *env) credentialContents(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, rel := range e.f.CredentialFiles {
		b, err := os.ReadFile(filepath.Join(e.home, rel)) //nolint:gosec // under the home of the check
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = b
	}
	return out
}

func filesDigest(t *testing.T, files []harness.File) string {
	t.Helper()
	d, err := harness.FilesDigest(files)
	if err != nil {
		t.Errorf("the files of a component are not a tree: %v", err)
	}
	return d
}

// applyRun applies s to a new, empty Run configuration directory, checks
// what is in it and returns it with its digest.
func (e *env) applyRun(t *testing.T, s harness.NativeSnapshot) (dir, digest string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var res harness.ApplyResult
	var err error
	e.homeUnchanged(t, "ApplyConfig to a TargetRun", func() {
		res, err = e.a.ApplyConfig(t.Context(), e.host, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true})
	})
	if err != nil {
		t.Fatalf("ApplyConfig to a TargetRun: %v", err)
	}
	if len(res.AuthLinks) == 0 {
		t.Error("ApplyConfig with LinkAuth linked no auth, but the home has credentials")
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if !slices.Contains(res.AuthLinks, rel) {
				t.Errorf("the Run configuration has the symlink %s, which is not an AuthLink", rel)
			}
		case d.Type().IsRegular():
			if slices.Contains(res.AuthLinks, rel) {
				t.Errorf("the auth %s is a copy, not a symlink", rel)
			}
			b, err := os.ReadFile(p) //nolint:gosec // the Run configuration of the check
			if err != nil {
				return err
			}
			if secret, leaked := e.leaks(b); leaked {
				t.Errorf("the Run configuration file %s carries the secret %q", rel, secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, err = treedigest.Dir(dir, treedigest.Options{ApprovedLinks: res.AuthLinks})
	if err != nil {
		t.Fatalf("the Run configuration has no digest: %v", err)
	}
	return dir, digest
}

// withRotatedRef is s with the first secret ref it has renamed.
func withRotatedRef(t *testing.T, s harness.NativeSnapshot) harness.NativeSnapshot {
	t.Helper()
	out := s
	out.Components = slices.Clone(s.Components)
	for i, c := range out.Components {
		for j, file := range c.Files {
			if len(file.SecretRefs) == 0 {
				continue
			}
			ref := file.SecretRefs[0]
			file.Content = bytes.ReplaceAll(file.Content, []byte(harness.SecretMarker(ref)), []byte(harness.SecretMarker(ref+"-rotated")))
			file.SecretRefs = harness.SecretRefs(file.Content)
			c.Files = slices.Clone(c.Files)
			c.Files[j] = file
			c.Digest = filesDigest(t, c.Files)
			out.Components[i] = c
			return out
		}
	}
	t.Fatal("the snapshot has no secret ref to rotate")
	return out
}

func checkApplyIsolated(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	if !e.caps.ConfigIsolation {
		s, ok := e.snapshot(t)
		if !ok {
			s = harness.NativeSnapshot{Harness: e.a.ID()}
		}
		dir := t.TempDir()
		var res harness.ApplyResult
		var err error
		unchanged(t, "an unsupported ApplyConfig to a TargetRun", dir, func() {
			e.homeUnchanged(t, "an unsupported ApplyConfig to a TargetRun", func() {
				res, err = e.a.ApplyConfig(t.Context(), e.host, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true})
			})
		})
		isUnsupported(t, "ApplyConfig to a TargetRun", res, err)
		return
	}
	s, ok := e.snapshot(t)
	if !ok {
		t.FailNow()
	}
	dry := t.TempDir()
	unchanged(t, "ApplyConfig with DryRun", dry, func() {
		if _, err := e.a.ApplyConfig(t.Context(), e.host, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: dry}, LinkAuth: true, DryRun: true}); err != nil {
			t.Errorf("ApplyConfig with DryRun: %v", err)
		}
	})
	_, first := e.applyRun(t, s)
	if _, again := e.applyRun(t, s); again != first {
		t.Errorf("the same snapshot gave the digests %s and %s", first, again)
	}
	e.rotateCredentials(t)
	if _, rotated := e.applyRun(t, s); rotated != first {
		t.Errorf("rotating the credentials changed the digest of the Run configuration: %s, then %s", first, rotated)
	}
	if _, ref := e.applyRun(t, withRotatedRef(t, s)); ref == first {
		t.Errorf("a different secret ref gave the same digest %s", first)
	}
	e.f.Mutate(t, e.home)
	mutated, _ := e.snapshot(t)
	if _, other := e.applyRun(t, mutated); other == first {
		t.Errorf("a different snapshot gave the same digest %s", first)
	}
}

// applyGlobal applies s to the home of e, and false when the adapter does
// not apply there.
func (e *env) applyGlobal(t *testing.T, s harness.NativeSnapshot) bool {
	t.Helper()
	credentials := map[string]manifest{}
	for _, rel := range e.f.CredentialFiles {
		credentials[rel] = takeManifest(t, filepath.Join(e.home, rel))
	}
	res, err := e.a.ApplyConfig(t.Context(), e.host, s, harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if errors.Is(err, harness.ErrUnsupported) {
		isUnsupported(t, "ApplyConfig to TargetGlobal", res, err)
		return false
	}
	if err != nil {
		t.Fatalf("ApplyConfig to TargetGlobal: %v", err)
	}
	for rel, before := range credentials {
		if d := before.diff(takeManifest(t, filepath.Join(e.home, rel))); d != "" {
			t.Errorf("ApplyConfig to TargetGlobal touched the credential file %s:\n%s", rel, d)
		}
	}
	return true
}

func component(s harness.NativeSnapshot, name string) (harness.Component, bool) {
	i := slices.IndexFunc(s.Components, func(c harness.Component) bool { return c.Name == name })
	if i < 0 {
		return harness.Component{}, false
	}
	return s.Components[i], true
}

func checkOwnership(t *testing.T, f Fixture) {
	t.Run("managed is restored", func(t *testing.T) {
		e := newEnv(t, f)
		s, ok := e.snapshot(t)
		if !ok {
			return
		}
		m := f.Mutate(t, e.home)
		if !e.applyGlobal(t, s) {
			return
		}
		if d, err := e.a.DiffConfig(t.Context(), e.host, s); err != nil || len(d.Changes) > 0 {
			t.Errorf("after applying the snapshot over the mutated %s, DiffConfig = %+v, %v; want no change", m, d, err)
		}
	})
	t.Run("local is kept", func(t *testing.T) {
		e := newEnv(t, f)
		s, ok := e.snapshot(t)
		if !ok {
			return
		}
		m := f.Mutate(t, e.home)
		mutated, _ := e.snapshot(t)
		want, found := component(mutated, m)
		local := s
		local.Components = slices.Clone(s.Components)
		i := slices.IndexFunc(local.Components, func(c harness.Component) bool { return c.Name == m })
		if i < 0 || !found {
			t.Fatalf("Mutate edited %q, which is not a component of the snapshot", m)
		}
		local.Components[i].Ownership = harness.Local
		if !e.applyGlobal(t, local) {
			return
		}
		after, _ := e.snapshot(t)
		if got, _ := component(after, m); !reflect.DeepEqual(got.Files, want.Files) {
			t.Errorf("applying %s as local overwrote it:\n%s\nwant\n%s", m, describe(got.Files), describe(want.Files))
		}
	})
	t.Run("json-subset keeps foreign fields", func(t *testing.T) {
		e := newEnv(t, f)
		s, ok := e.snapshot(t)
		if !ok {
			return
		}
		i := slices.IndexFunc(s.Components, func(c harness.Component) bool { return c.Kind == harness.KindJSONSubset })
		if i < 0 {
			t.Log("the snapshot has no json-subset component")
			return
		}
		orig := s.Components[i]
		fields := jsonFields(t, orig.Files[0].Content)
		keys := slices.Sorted(maps.Keys(fields))
		if len(keys) < 2 {
			t.Fatalf("json-subset %s has %d field; the fixture must seed two or more", orig.Name, len(keys))
		}
		subset := orig
		subset.Files = slices.Clone(orig.Files)
		subset.Files[0].Content = []byte(`{"` + keys[0] + `":"harnesstest"}`)
		subset.Files[0].SecretRefs = nil
		subset.Digest = filesDigest(t, subset.Files)
		partial := s
		partial.Components = slices.Clone(s.Components)
		partial.Components[i] = subset
		if !e.applyGlobal(t, partial) {
			return
		}
		after, _ := e.snapshot(t)
		got, _ := component(after, orig.Name)
		gotFields := jsonFields(t, got.Files[0].Content)
		if v := gotFields[keys[0]]; v != "harnesstest" {
			t.Errorf("json-subset %s: the applied field %s is %v", orig.Name, keys[0], v)
		}
		for _, k := range keys[1:] {
			if !reflect.DeepEqual(gotFields[k], fields[k]) {
				t.Errorf("json-subset %s: the foreign field %s went from %v to %v", orig.Name, k, fields[k], gotFields[k])
			}
		}
	})
}

func jsonFields(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("a json-subset is not a JSON object: %v", err)
	}
	return obj
}

func checkDiff(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	s, ok := e.snapshot(t)
	if !ok {
		return
	}
	var d harness.ConfigDiff
	var err error
	e.homeUnchanged(t, "DiffConfig", func() { d, err = e.a.DiffConfig(t.Context(), e.host, s) })
	if errors.Is(err, harness.ErrUnsupported) {
		isUnsupported(t, "DiffConfig", d, err)
		return
	}
	if err != nil || len(d.Changes) > 0 {
		t.Errorf("DiffConfig right after SnapshotConfig = %+v, %v; want no change", d, err)
	}
	m := f.Mutate(t, e.home)
	d, err = e.a.DiffConfig(t.Context(), e.host, s)
	if err != nil {
		t.Fatalf("DiffConfig after Mutate: %v", err)
	}
	if !slices.ContainsFunc(d.Changes, func(c harness.Change) bool { return c.Name == m }) {
		t.Errorf("DiffConfig after mutating %s = %+v; want a change of %s", m, d.Changes, m)
	}
}
