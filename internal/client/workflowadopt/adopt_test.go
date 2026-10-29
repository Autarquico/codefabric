package workflowadopt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

// fakeStore is a Controller in memory: a CAS that checks digests, a commit
// that checks the parent and that the blobs are there, and a log of every
// byte that reached it.
type fakeStore struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	versions map[string][]v1.WorkflowVersion
	puts     int
	commits  int
	// wire is everything received: blobs and manifests.
	wire [][]byte
	// beforeCommit runs when a commit arrives, to move the head under it.
	beforeCommit func()
}

func newStore() *fakeStore {
	return &fakeStore{blobs: map[string][]byte{}, versions: map[string][]v1.WorkflowVersion{}}
}

func (s *fakeStore) HasBlob(_ context.Context, d string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[d]
	return ok, nil
}

func (s *fakeStore) PutBlob(_ context.Context, d string, r io.Reader, size int64) (v1.BlobMeta, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return v1.BlobMeta{}, err
	}
	sum := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(sum[:]) != d || int64(len(b)) != size {
		return v1.BlobMeta{}, domain.NewError(domain.CodeInvalidRequest, "digest mismatch", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.blobs[d] = b
	s.wire = append(s.wire, b)
	return v1.BlobMeta{Digest: d, Size: size}, nil
}

func (s *fakeStore) CommitWorkflow(_ context.Context, name string, req v1.CommitWorkflowRequest) (v1.WorkflowVersion, error) {
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	raw, _ := json.Marshal(req)
	s.wire = append(s.wire, raw)
	head := len(s.versions[name])
	if req.Parent != head {
		return v1.WorkflowVersion{}, domain.NewError(domain.CodeWorkflowConflict, "stale parent", map[string]any{"head": head})
	}
	if err := req.Manifest.Validate(); err != nil {
		return v1.WorkflowVersion{}, err
	}
	for _, c := range req.Manifest.Components {
		for _, f := range c.Files {
			if _, ok := s.blobs[f.SHA256]; !ok {
				return v1.WorkflowVersion{}, domain.NewError(domain.CodeInvalidRequest, "missing blob", nil)
			}
		}
	}
	digest, _ := req.Manifest.Digest()
	m := req.Manifest
	v := v1.WorkflowVersion{Workflow: name, Version: head + 1, Parent: req.Parent, Digest: digest, Manifest: &m}
	s.versions[name] = append(s.versions[name], v)
	return v, nil
}

func (s *fakeStore) ShowWorkflow(_ context.Context, ref string) (v1.WorkflowVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.versions[ref]
	if len(vs) == 0 {
		return v1.WorkflowVersion{}, domain.NewError(domain.CodeNotFound, "no such workflow", nil)
	}
	return vs[len(vs)-1], nil
}

// everything is all the bytes the store saw, as one string.
func (s *fakeStore) everything() string { return string(bytes.Join(s.wire, []byte("\n"))) }

// escaping is the fake harness with markers rendered raw in settings.json.
type escaping struct{ fakeharness.Adapter }

func (escaping) SecretEscape(path string) (harness.Escape, error) {
	if path != fakeharness.SettingsFile {
		return "", errors.New("no secrets here")
	}
	return harness.EscapeRaw, nil
}

func hostAt(t *testing.T, home string) harness.Host {
	t.Helper()
	h, err := localhost.New(localhost.Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func registry(t *testing.T, ads ...harness.Adapter) *harness.Registry {
	t.Helper()
	r, err := harness.NewRegistry(ads...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func write(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

const fakeCredentials = `{"access_token":"fake-credential-do-not-upload"}`

// fakeHome is a ~/.fakeharness with settings (with MCP), a script, two
// skills and the auth file, which no snapshot may carry.
func fakeHome(t *testing.T, settings string) string {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, fakeharness.ConfigDirName)
	write(t, root, fakeharness.SettingsFile, settings, 0o644)
	write(t, root, fakeharness.ScriptFile, `{"steps":[]}`, 0o644)
	write(t, root, "skills/review/SKILL.md", "# review\n", 0o644)
	write(t, root, "skills/review/run.sh", "#!/bin/sh\n", 0o755)
	write(t, root, "skills/twin/SKILL.md", "# review\n", 0o644)
	write(t, root, fakeharness.AuthFile, fakeCredentials, 0o600)
	return home
}

const cleanSettings = `{"model":"m","mcpServers":{"docs":{"command":"docs-mcp","args":["-y"]}}}`

func codexFixture(t *testing.T) (home, authContent string) {
	t.Helper()
	home, err := filepath.Abs("../../adapters/harness/codex/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(filepath.Join(home, codex.ConfigDirName, codex.AuthFile)) //nolint:gosec // fixed test fixture
	if err != nil || len(auth) == 0 {
		t.Fatalf("fixture auth: %v", err)
	}
	return home, string(auth)
}

func names(m domain.Manifest, h string) []string {
	var out []string
	for _, c := range m.Components {
		if c.Harness == h {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestScenario_Adopt(t *testing.T) {
	t.Run("codex real adapter", func(t *testing.T) {
		home, auth := codexFixture(t)
		store := newStore()
		a := &Adopter{Registry: registry(t, codex.Adapter{}), Host: hostAt(t, home), Store: store, Confirmer: &acceptAll{}}
		v, err := a.Adopt(context.Background(), "team", []Selection{{Harness: "codex"}}, "first")
		if err != nil {
			t.Fatal(err)
		}
		if v.Version != 1 || v.Parent != 0 {
			t.Fatalf("version = %+v", v)
		}
		got := names(*v.Manifest, "codex")
		for _, want := range []string{codex.ConfigComponent, codex.AgentsComponent, codex.PromptsComponent, codex.SkillsComponent} {
			if !contains(got, want) {
				t.Errorf("components %v lack %s", got, want)
			}
		}
		assertNoAuth(t, store, *v.Manifest, auth, codex.AuthFile)
		// The fixture's secrets reach the Controller as refs only.
		for _, leaked := range []string{"sk-docs-secret-1", "web-secret-2", "odd-secret-3", "corp-secret-4", "gh-secret-6"} {
			if strings.Contains(store.everything(), leaked) {
				t.Errorf("%s reached the store", leaked)
			}
		}
		assertDigests(t, store, *v.Manifest)
	})

	t.Run("fake harness settings skills and mcp", func(t *testing.T) {
		home := fakeHome(t, cleanSettings)
		store := newStore()
		a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, home), Store: store}
		v, err := a.Adopt(context.Background(), "team", []Selection{{Harness: fakeharness.DefaultID}}, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"script", "settings", "skills/review", "skills/twin"}
		if got := names(*v.Manifest, fakeharness.DefaultID); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("components = %v, want %v", got, want)
		}
		assertNoAuth(t, store, *v.Manifest, fakeCredentials, fakeharness.AuthFile)
		assertDigests(t, store, *v.Manifest)
		for _, c := range v.Manifest.Components {
			if c.Name != "skills/review" {
				continue
			}
			for _, f := range c.Files {
				if f.Exec != (f.Path == "skills/review/run.sh") {
					t.Errorf("exec bit of %s = %v", f.Path, f.Exec)
				}
			}
		}
		// skills/review/SKILL.md and skills/twin/SKILL.md are one blob.
		if store.puts != len(store.blobs) || len(store.blobs) != 4 {
			t.Errorf("puts = %d, blobs = %d; want 4 distinct", store.puts, len(store.blobs))
		}
	})

	t.Run("selection narrows the snapshot", func(t *testing.T) {
		home := fakeHome(t, cleanSettings)
		a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, home), Store: newStore()}
		v, err := a.Adopt(context.Background(), "team", []Selection{{Harness: fakeharness.DefaultID, Components: []string{"settings"}}}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := names(*v.Manifest, fakeharness.DefaultID); len(got) != 1 || got[0] != "settings" {
			t.Fatalf("components = %v", got)
		}
	})

	t.Run("an existing blob is not sent again", func(t *testing.T) {
		home := fakeHome(t, cleanSettings)
		store := newStore()
		a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, home), Store: store}
		if _, err := a.Adopt(context.Background(), "one", []Selection{{Harness: fakeharness.DefaultID}}, ""); err != nil {
			t.Fatal(err)
		}
		puts := store.puts
		if _, err := a.Adopt(context.Background(), "two", []Selection{{Harness: fakeharness.DefaultID}}, ""); err != nil {
			t.Fatal(err)
		}
		if store.puts != puts {
			t.Errorf("puts went from %d to %d", puts, store.puts)
		}
	})

	t.Run("an adapter that snapshots an auth file is refused", func(t *testing.T) {
		home := fakeHome(t, cleanSettings)
		store := newStore()
		a := &Adopter{Registry: registry(t, leaky{fakeharness.Adapter{}}), Host: hostAt(t, home), Store: store}
		_, err := a.Adopt(context.Background(), "team", []Selection{{Harness: fakeharness.DefaultID}}, "")
		if !errors.Is(err, ErrAuthInSnapshot) {
			t.Fatalf("err = %v", err)
		}
		if store.puts+store.commits != 0 {
			t.Errorf("sent %d blobs, %d commits", store.puts, store.commits)
		}
		if strings.Contains(err.Error(), "fake-credential") {
			t.Errorf("error carries credential: %v", err)
		}
	})

	t.Run("unknown harness", func(t *testing.T) {
		a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, t.TempDir()), Store: newStore()}
		if _, err := a.Adopt(context.Background(), "team", []Selection{{Harness: "nope"}}, ""); err == nil {
			t.Fatal("want an error")
		}
	})
}

// leaky is an adapter with a bug: its snapshot carries the auth file.
type leaky struct{ fakeharness.Adapter }

func (l leaky) SnapshotConfig(ctx context.Context, h harness.Host, o harness.SnapshotOptions) (harness.NativeSnapshot, error) {
	snap, err := l.Adapter.SnapshotConfig(ctx, h, o)
	if err != nil {
		return snap, err
	}
	f := harness.File{Path: "sub/" + fakeharness.AuthFile, Mode: 0o600, Content: []byte(fakeCredentials)}
	snap.Components[0].Files = append(snap.Components[0].Files, f)
	return snap, nil
}

func contains(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

// assertNoAuth: no manifest file is the auth file and no byte of its content
// reached the store.
func assertNoAuth(t *testing.T, s *fakeStore, m domain.Manifest, auth, authFile string) {
	t.Helper()
	for _, c := range m.Components {
		for _, f := range c.Files {
			if filepath.Base(f.Path) == authFile {
				t.Errorf("manifest has %s/%s", c.Name, f.Path)
			}
		}
	}
	if strings.Contains(s.everything(), strings.TrimSpace(auth)) {
		t.Errorf("auth content reached the store")
	}
}

// assertDigests: every file names a stored blob with that digest, and every
// component's tree_digest is that of its files.
func assertDigests(t *testing.T, s *fakeStore, m domain.Manifest) {
	t.Helper()
	for _, c := range m.Components {
		var files []harness.File
		for _, f := range c.Files {
			b, ok := s.blobs[f.SHA256]
			if !ok {
				t.Fatalf("%s/%s: blob %s missing", c.Name, f.Path, f.SHA256)
			}
			mode := os.FileMode(0o644)
			if f.Exec {
				mode = 0o755
			}
			files = append(files, harness.File{Path: f.Path, Mode: mode, Content: b})
		}
		d, err := harness.FilesDigest(files)
		if err != nil || d != c.TreeDigest {
			t.Errorf("%s: tree_digest %s, want %s (%v)", c.Name, c.TreeDigest, d, err)
		}
	}
}

// recording is a Confirmer that keeps what it was shown.
type recording struct {
	decision func([]HarnessSecrets) bool
	seen     []HarnessSecrets
}

func (r *recording) ConfirmSecrets(_ context.Context, found []HarnessSecrets) (bool, error) {
	r.seen = found
	return r.decision(found), nil
}

func (r *recording) ConfirmChanges(context.Context, Review) (bool, error) { return true, nil }

// acceptAll confirms whatever it is shown.
type acceptAll struct{ reviews []Review }

func (*acceptAll) ConfirmSecrets(context.Context, []HarnessSecrets) (bool, error) { return true, nil }

func (a *acceptAll) ConfirmChanges(_ context.Context, r Review) (bool, error) {
	a.reviews = append(a.reviews, r)
	return true, nil
}

// refuseAll declines whatever it is shown.
type refuseAll struct{}

func (refuseAll) ConfirmSecrets(context.Context, []HarnessSecrets) (bool, error) { return false, nil }

func (refuseAll) ConfirmChanges(context.Context, Review) (bool, error) { return false, nil }

func TestScenario_SecretosEnSettings(t *testing.T) {
	token := "ghp" + "_" + "aB3dE5gH7jK9mN1pQ3sT5vX7zA9cD1fG3hJ5"
	settings := `{"model":"m","mcpServers":{"gh":{"command":"gh-mcp","env":{"GITHUB_TOKEN":"` + token + `"}}}}`
	sel := []Selection{{Harness: fakeharness.DefaultID}}
	adapters := func(t *testing.T) *harness.Registry { return registry(t, escaping{fakeharness.Adapter{}}) }

	t.Run("confirmed: replaced by a ref, the value goes nowhere", func(t *testing.T) {
		home := fakeHome(t, settings)
		store := newStore()
		rec := &recording{decision: func([]HarnessSecrets) bool { return true }}
		a := &Adopter{Registry: adapters(t), Host: hostAt(t, home), Store: store, Confirmer: rec}
		v, err := a.Adopt(context.Background(), "team", sel, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rec.seen) != 1 || len(rec.seen[0].Hits) != 1 || rec.seen[0].Hits[0].Kind != "github-token" {
			t.Fatalf("the confirmer saw %+v", rec.seen)
		}
		raw, _ := json.Marshal(v)
		if strings.Contains(store.everything(), token) || strings.Contains(string(raw), token) {
			t.Fatal("the token reached the store")
		}
		var refs []string
		for _, c := range v.Manifest.Components {
			for _, f := range c.Files {
				refs = append(refs, f.SecretRefs...)
			}
		}
		if len(refs) != 1 || !strings.Contains(store.everything(), harness.SecretMarker(refs[0])) {
			t.Fatalf("refs = %v", refs)
		}
		assertDigests(t, store, *v.Manifest)
	})

	t.Run("declined", func(t *testing.T) {
		store := newStore()
		rec := &recording{decision: func([]HarnessSecrets) bool { return false }}
		a := &Adopter{Registry: adapters(t), Host: hostAt(t, fakeHome(t, settings)), Store: store, Confirmer: rec}
		_, err := a.Adopt(context.Background(), "team", sel, "")
		assertAborted(t, store, err, ErrSecretsUnconfirmed, token)
	})

	t.Run("headless without accept-all", func(t *testing.T) {
		store := newStore()
		a := &Adopter{Registry: adapters(t), Host: hostAt(t, fakeHome(t, settings)), Store: store}
		_, err := a.Adopt(context.Background(), "team", sel, "")
		assertAborted(t, store, err, ErrSecretsUnconfirmed, token)
	})

	t.Run("adapter without markers in that file", func(t *testing.T) {
		store := newStore()
		a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, fakeHome(t, settings)), Store: store, Confirmer: &acceptAll{}}
		_, err := a.Adopt(context.Background(), "team", sel, "")
		assertAborted(t, store, err, ErrNotReplaceable, token)
	})

	t.Run("codex: a hit outside config.toml is not replaceable", func(t *testing.T) {
		home := t.TempDir()
		write(t, filepath.Join(home, codex.ConfigDirName), codex.AgentsComponent, "use "+token+" for gh\n", 0o644)
		store := newStore()
		a := &Adopter{Registry: registry(t, codex.Adapter{}), Host: hostAt(t, home), Store: store, Confirmer: &acceptAll{}}
		_, err := a.Adopt(context.Background(), "team", []Selection{{Harness: "codex"}}, "")
		assertAborted(t, store, err, ErrNotReplaceable, token)
	})

	t.Run("a hit in MCP args is not replaceable", func(t *testing.T) {
		args := `{"mcpServers":{"gh":{"command":"gh-mcp","args":["--token","` + token + `"]}}}`
		store := newStore()
		a := &Adopter{Registry: adapters(t), Host: hostAt(t, fakeHome(t, args)), Store: store, Confirmer: &acceptAll{}}
		_, err := a.Adopt(context.Background(), "team", sel, "")
		assertAborted(t, store, err, ErrNotReplaceable, token)
	})
}

func assertAborted(t *testing.T, s *fakeStore, err, want error, value string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if s.puts+s.commits != 0 {
		t.Errorf("uploaded %d blobs and %d commits", s.puts, s.commits)
	}
	if strings.Contains(err.Error(), value) || strings.Contains(s.everything(), value) {
		t.Error("the value leaked")
	}
}

func isConflict(err error) (head int, ok bool) {
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeWorkflowConflict {
		return 0, false
	}
	h, _ := de.Details["head"].(int)
	return h, true
}

func TestCommitConflictIsTheDomainError(t *testing.T) {
	home := fakeHome(t, cleanSettings)
	a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, home), Store: newStore()}
	sel := []Selection{{Harness: fakeharness.DefaultID}}
	if _, err := a.Adopt(context.Background(), "team", sel, ""); err != nil {
		t.Fatal(err)
	}
	_, err := a.Adopt(context.Background(), "team", sel, "")
	if head, ok := isConflict(err); !ok || head != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestCommitOnHead(t *testing.T) {
	sel := []Selection{{Harness: fakeharness.DefaultID}}
	setup := func(t *testing.T) (*Adopter, *fakeStore, string) {
		home := fakeHome(t, cleanSettings)
		store := newStore()
		a := &Adopter{Registry: registry(t, fakeharness.Adapter{}), Host: hostAt(t, home), Store: store}
		p, err := a.Prepare(context.Background(), sel)
		if err != nil {
			t.Fatal(err)
		}
		for i := range p.Manifest.Components {
			if p.Manifest.Components[i].Name == "settings" {
				p.Manifest.Components[i].Ownership = domain.OwnershipLocal
			}
		}
		if _, err := a.Commit(context.Background(), "team", p, 0, ""); err != nil {
			t.Fatal(err)
		}
		return a, store, home
	}
	advance := func(t *testing.T, a *Adopter) {
		// Someone else's commit: a different snapshot on top of the head.
		other := hostAt(t, fakeHome(t, `{"model":"other"}`))
		b := &Adopter{Registry: a.Registry, Host: other, Store: a.Store}
		p, err := b.Prepare(context.Background(), sel)
		if err != nil {
			t.Fatal(err)
		}
		head, err := a.Store.ShowWorkflow(context.Background(), "team")
		if err != nil {
			t.Fatal(err)
		}
		keepOwnership(&p.Manifest, *head.Manifest)
		if _, err := b.Commit(context.Background(), "team", p, head.Version, ""); err != nil {
			t.Fatal(err)
		}
	}
	ownership := func(t *testing.T, store *fakeStore, name string) domain.Ownership {
		head, _ := store.ShowWorkflow(context.Background(), "team")
		for _, c := range head.Manifest.Components {
			if c.Name == name {
				return c.Ownership
			}
		}
		t.Fatalf("no component %s", name)
		return ""
	}

	t.Run("commits on the head with the diff confirmed", func(t *testing.T) {
		a, store, home := setup(t)
		write(t, filepath.Join(home, fakeharness.ConfigDirName), "skills/review/SKILL.md", "# changed\n", 0o644)
		c := &acceptAll{}
		got, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{Changelog: "next", Confirmer: c})
		if err != nil || got.Unchanged || got.Version.Version != 2 || got.Version.Parent != 1 {
			t.Fatalf("got %+v, %v", got, err)
		}
		if len(c.reviews) != 1 || c.reviews[0].Head != 1 || c.reviews[0].Attempt != 0 || len(c.reviews[0].Diff) != 1 || c.reviews[0].Diff[0].Name != "skills/review" {
			t.Errorf("reviews = %+v", c.reviews)
		}
	})

	t.Run("a component that was local stays local", func(t *testing.T) {
		a, store, home := setup(t)
		write(t, filepath.Join(home, fakeharness.ConfigDirName), fakeharness.SettingsFile, `{"model":"changed"}`, 0o644)
		if _, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{Confirmer: &acceptAll{}}); err != nil {
			t.Fatal(err)
		}
		if got := ownership(t, store, "settings"); got != domain.OwnershipLocal {
			t.Errorf("settings is %s after the commit, want local", got)
		}
	})

	t.Run("a rebase keeps the ownership of the head it lands on", func(t *testing.T) {
		a, store, _ := setup(t)
		advance(t, a) // v2: someone else's settings, still local
		store.beforeCommit = func() {
			store.beforeCommit = nil
			advance(t, a) // v3 arrives under our first commit
		}
		c := &acceptAll{}
		got, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{Rebase: true, Confirmer: c})
		if err != nil || got.Version.Version != 4 || got.Version.Parent != 3 {
			t.Fatalf("got %+v, %v", got, err)
		}
		if len(c.reviews) != 2 || c.reviews[1].Attempt != 1 || c.reviews[1].Head != 3 {
			t.Errorf("reviews = %+v: want a second look at the new head", c.reviews)
		}
		if got := ownership(t, store, "settings"); got != domain.OwnershipLocal {
			t.Errorf("settings is %s after the rebase, want local", got)
		}
	})

	t.Run("without Rebase a conflict is returned", func(t *testing.T) {
		a, store, _ := setup(t)
		advance(t, a)
		store.beforeCommit = func() {
			store.beforeCommit = nil
			advance(t, a)
		}
		_, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{Confirmer: &acceptAll{}})
		if head, ok := isConflict(err); !ok || head != 3 {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a second conflict is returned as it is", func(t *testing.T) {
		a, store, _ := setup(t)
		advance(t, a)
		moves := 0
		var hook func()
		hook = func() {
			if moves++; moves <= 2 {
				store.beforeCommit = nil
				advance(t, a)
				store.beforeCommit = hook
			}
		}
		store.beforeCommit = hook
		_, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{Rebase: true, Confirmer: &acceptAll{}})
		if head, ok := isConflict(err); !ok || head != 4 || moves != 2 {
			t.Fatalf("err = %v, moves %d", err, moves)
		}
	})

	t.Run("declined commits nothing", func(t *testing.T) {
		a, store, _ := setup(t)
		advance(t, a)
		commits := store.commits
		_, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{Confirmer: refuseAll{}})
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeConfirmationRequired || store.commits != commits {
			t.Fatalf("err = %v, commits %d -> %d", err, commits, store.commits)
		}
	})

	t.Run("nothing new is not a commit and needs no confirmer", func(t *testing.T) {
		a, store, _ := setup(t)
		got, err := CommitOnHead(context.Background(), a, store, "team", CommitOptions{})
		if err != nil || !got.Unchanged || got.Version.Version != 1 || store.commits != 1 {
			t.Fatalf("got %+v, %v, commits %d", got, err, store.commits)
		}
	})
}

func TestSelectionOf(t *testing.T) {
	comp := func(name string) domain.ManifestComponent { return domain.ManifestComponent{Harness: "h", Name: name} }
	m := domain.Manifest{Components: []domain.ManifestComponent{comp("new"), comp("edit"), comp("keep")}}
	if sel := SelectionOf(m); len(sel) != 1 || strings.Join(sel[0].Components, ",") != "edit,keep,new" {
		t.Errorf("selection = %+v", sel)
	}
}
