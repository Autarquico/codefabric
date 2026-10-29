package app_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/workflows"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// wfEnv is an env with a real blob store below its own directory.
type wfEnv struct {
	*env
	cas string
}

func newWorkflowEnv(t *testing.T, maxBlob int64) *wfEnv {
	t.Helper()
	cas := t.TempDir()
	e := newEnvWith(t, func(c *app.Config, clk *clock.Fake) {
		store, err := blob.New(cas, clk)
		must(t, err)
		c.Blobs, c.MaxBlobBytes = store, maxBlob
	})
	return &wfEnv{env: e, cas: cas}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// upload puts content in the blob store and returns its digest.
func (e *wfEnv) upload(content string) string {
	e.t.Helper()
	_, err := e.app.Workflows.PutBlob(e.t.Context(), e.owner, sha(content), strings.NewReader(content))
	must(e.t, err)
	return sha(content)
}

// component is a component of one file whose blob is uploaded and whose
// tree_digest is right.
func (e *wfEnv) component(harness, name, content string) domain.ManifestComponent {
	e.t.Helper()
	tree, err := treedigest.Files([]treedigest.Entry{treedigest.File{Path: name + ".md", Content: []byte(content)}})
	must(e.t, err)
	return domain.ManifestComponent{
		Harness: harness, Name: name, Kind: "file", Ownership: domain.OwnershipManaged, TreeDigest: tree,
		Files: []domain.ManifestFile{{Path: name + ".md", SHA256: e.upload(content), SecretRefs: []string{}}},
	}
}

func manifest(cs ...domain.ManifestComponent) domain.Manifest {
	return domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Components: cs}
}

func (e *wfEnv) commit(name string, parent int, m domain.Manifest, mutate ...func(*workflows.CommitRequest)) (workflows.Version, error) {
	req := workflows.CommitRequest{Name: name, Parent: parent, Manifest: m, Changelog: "msg"}
	for _, f := range mutate {
		f(&req)
	}
	return e.app.Workflows.Commit(e.t.Context(), e.owner, req)
}

func (e *wfEnv) mustCommit(name string, parent int, m domain.Manifest) workflows.Version {
	e.t.Helper()
	v, err := e.commit(name, parent, m)
	must(e.t, err)
	return v
}

func TestWorkflows_AdoptThenCommitRecordsVersionsAndChanges(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	v1 := e.mustCommit("review", 0, manifest(e.component("codex", "agents", "one"), e.component("codex", "rules", "r")))
	if v1.Version != 1 || v1.Parent != 0 || v1.Origin != "client" || v1.Author.ID != string(e.owner.Principal.ID) || len(v1.Changed) != 2 {
		t.Fatalf("v1 = %+v", v1)
	}
	for _, c := range v1.Changed {
		if c.Change != "added" {
			t.Errorf("v1 change = %+v", c)
		}
	}
	v2 := e.mustCommit("review", 1, manifest(e.component("codex", "agents", "two"), e.component("claude", "notes", "n")))
	if v2.Version != 2 || v2.Parent != 1 || v2.Digest == v1.Digest {
		t.Fatalf("v2 = %+v", v2)
	}
	got := map[string]string{}
	for _, c := range v2.Changed {
		got[c.Harness+"/"+c.Name] = c.Change
	}
	want := map[string]string{"codex/agents": "modified", "codex/rules": "removed", "claude/notes": "added"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("changed = %v, want %v", got, want)
	}

	if ev := e.events(domain.EventWorkflowAdopted); len(ev) != 1 || ev[0].WorkflowID != domain.ID(v1.WorkflowID) {
		t.Errorf("adopted events = %+v", ev)
	}
	if ev := e.events(domain.EventWorkflowCommitted); len(ev) != 1 || ev[0].Data["version"] != float64(2) {
		t.Errorf("committed events = %+v", ev)
	}

	list, err := e.app.Workflows.List(t.Context())
	must(t, err)
	if len(list) != 1 || list[0].Name != "review" || list[0].Head != 2 || list[0].HeadDigest != v2.Digest {
		t.Errorf("list = %+v", list)
	}
	hist, err := e.app.Workflows.History(t.Context(), "review")
	must(t, err)
	if len(hist) != 2 || hist[0].Version != 2 || hist[1].Version != 1 || hist[0].Manifest != nil {
		t.Errorf("history = %+v", hist)
	}
	shown, err := e.app.Workflows.Show(t.Context(), "review@1")
	must(t, err)
	if shown.Digest != v1.Digest || shown.Manifest == nil || len(shown.Manifest.Components) != 2 {
		t.Errorf("show = %+v", shown)
	}
	if digest, err := shown.Manifest.Digest(); err != nil || digest != v1.Digest {
		t.Errorf("the stored manifest digests to %s, %v; want %s", digest, err, v1.Digest)
	}
}

// Two commits on the same parent: one wins, the other gets CF_WORKFLOW_CONFLICT
// and the conflict is an event.
func TestScenario_Conflicto(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.mustCommit("review", 0, manifest(e.component("codex", "agents", "base")))

	const n = 6
	ms := make([]domain.Manifest, n)
	for i := range ms {
		ms[i] = manifest(e.component("codex", "agents", fmt.Sprintf("edit %d", i)))
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.commit("review", 1, ms[i])
		}()
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		wantCode(t, err, domain.CodeWorkflowConflict)
		if domain.ExitCode(domain.CodeWorkflowConflict) != 4 {
			t.Fatal("conflict is not exit 4")
		}
	}
	if wins != 1 {
		t.Fatalf("%d commits won, want exactly 1 (errors: %v)", wins, errs)
	}
	if ev := e.events(domain.EventWorkflowConflict); len(ev) != n-1 || ev[0].Data["head"] != float64(2) || ev[0].Data["parent"] != float64(1) {
		t.Errorf("conflict events = %+v", ev)
	}
	if hist, _ := e.app.Workflows.History(t.Context(), "review"); len(hist) != 2 {
		t.Errorf("history has %d versions, want 2", len(hist))
	}
}

func TestWorkflows_ParentRules(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	m1, m2 := manifest(e.component("codex", "a", "1")), manifest(e.component("codex", "a", "2"))

	_, err := e.commit("review", 3, m1)
	wantCode(t, err, domain.CodeNotFound) // a parent on a Workflow that does not exist
	e.mustCommit("review", 0, m1)
	_, err = e.commit("review", 0, m2)
	wantCode(t, err, domain.CodeWorkflowConflict) // adopting a name in use
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "cf workflow commit") {
		t.Errorf("adopting a name in use says %q", err)
	}
	_, err = e.commit("review", 2, m2)
	wantCode(t, err, domain.CodeWorkflowConflict) // a parent in the future
	_, err = e.commit("review", -1, m2)
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestWorkflows_NothingToCommitButARevertIsAVersion(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	a, b := manifest(e.component("codex", "a", "1")), manifest(e.component("codex", "a", "2"))
	e.mustCommit("review", 0, a)

	_, err := e.commit("review", 1, a)
	wantCode(t, err, domain.CodeInvalidRequest)
	var de *domain.Error
	if !errors.As(err, &de) || de.Details["nothing_to_commit"] != true || !strings.Contains(de.Message, "nothing to commit") {
		t.Errorf("err = %v", err)
	}
	e.mustCommit("review", 1, b)
	v3, err := e.commit("review", 2, a)
	if err != nil || v3.Version != 3 {
		t.Fatalf("revert = %+v, %v", v3, err)
	}
	// The digest of the reverted content resolves to the latest version that has it.
	ref, err := e.app.Workflows.Resolve(t.Context(), "review@"+v3.Digest)
	if err != nil || ref.Version != 3 {
		t.Errorf("by digest = %+v, %v", ref, err)
	}
}

func TestWorkflows_IntegrityOfACommit(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	good := e.component("codex", "agents", "content")

	cases := map[string]func(c *domain.ManifestComponent){
		"blob never uploaded": func(c *domain.ManifestComponent) { c.Files[0].SHA256 = sha("never uploaded") },
		"wrong tree_digest":   func(c *domain.ManifestComponent) { c.TreeDigest = sha("something else") },
		"declared exec bit":   func(c *domain.ManifestComponent) { c.Files[0].Exec = true }, // part of the tree digest
		"parent path":         func(c *domain.ManifestComponent) { c.Files[0].Path = "../escape.md" },
		"absolute path":       func(c *domain.ManifestComponent) { c.Files[0].Path = "/etc/passwd" },
		"duplicate path": func(c *domain.ManifestComponent) {
			c.Files = append(c.Files, c.Files[0])
		},
		"malformed sha256": func(c *domain.ManifestComponent) { c.Files[0].SHA256 = "sha256:xyz" },
	}
	for name, mutate := range cases {
		c := good
		c.Files = append([]domain.ManifestFile(nil), good.Files...)
		mutate(&c)
		_, err := e.commit("review", 0, manifest(c))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	if _, err := e.app.Workflows.Show(t.Context(), "review"); err == nil {
		t.Error("a rejected commit left a Workflow behind")
	}
	if n := e.count("workflows"); n != 0 {
		t.Errorf("%d workflows after rejected commits", n)
	}

	// A blob whose bytes were changed on disk no longer hashes to its name.
	hexPart := strings.TrimPrefix(good.Files[0].SHA256, "sha256:")
	path := filepath.Join(e.cas, "blobs", "sha256", hexPart[:2], hexPart[2:4], hexPart)
	must(t, os.Chmod(path, 0o600))
	must(t, os.WriteFile(path, []byte("tampered"), 0o600))
	_, err := e.commit("review", 0, manifest(good))
	wantCode(t, err, domain.CodeInvalidRequest)
	if !strings.Contains(err.Error(), "does not hash") {
		t.Errorf("err = %v", err)
	}
}

func TestWorkflows_CommitInputs(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	m := manifest(e.component("codex", "a", "1"))
	for name, mutate := range map[string]func(*workflows.CommitRequest){
		"upper-case name": func(r *workflows.CommitRequest) { r.Name = "Review" },
		"id-like name": func(r *workflows.CommitRequest) {
			r.Name = string(domain.NewIDGenerator(e.clock.Now, bytes.NewReader(make([]byte, 64))).New(domain.KindProject))
		},
		"empty manifest": func(r *workflows.CommitRequest) { r.Manifest = domain.Manifest{} },
		"long changelog": func(r *workflows.CommitRequest) { r.Changelog = strings.Repeat("x", 5000) },
		"bad request id": func(r *workflows.CommitRequest) { r.RequestID = "has space" },
	} {
		_, err := e.commit("review", 0, m, mutate)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
}

func TestWorkflows_SameRequestIDReplaysTheCommit(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	m := manifest(e.component("codex", "a", "1"))
	withID := func(r *workflows.CommitRequest) { r.RequestID = "req-1" }

	first, err := e.commit("review", 0, m, withID)
	must(t, err)
	again, err := e.commit("review", 0, m, withID)
	must(t, err)
	if again.Version != 1 || again.Digest != first.Digest || again.WorkflowID != first.WorkflowID {
		t.Errorf("replay = %+v, first = %+v", again, first)
	}
	if n := len(e.events(domain.EventWorkflowAdopted)); n != 1 {
		t.Errorf("%d adopted events after a replay, want 1", n)
	}
	_, err = e.commit("review", 0, manifest(e.component("codex", "a", "other")), withID)
	wantCode(t, err, domain.CodeIdempotencyMismatch)
}

func TestWorkflows_ResolveSelectors(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	v1 := e.mustCommit("review", 0, manifest(e.component("codex", "a", "1")))
	v2 := e.mustCommit("review", 1, manifest(e.component("codex", "a", "2")))
	id := domain.ID(v1.WorkflowID)

	cases := []struct {
		selector string
		want     domain.WorkflowRef
		code     domain.Code
	}{
		{"review", domain.WorkflowRef{ID: id, Version: 2, Digest: v2.Digest}, ""},
		{"review@latest", domain.WorkflowRef{ID: id, Version: 2, Digest: v2.Digest}, ""},
		{"review@1", domain.WorkflowRef{ID: id, Version: 1, Digest: v1.Digest}, ""},
		{"review@" + v1.Digest, domain.WorkflowRef{ID: id, Version: 1, Digest: v1.Digest}, ""},
		{"review@3", domain.WorkflowRef{}, domain.CodeNotFound},
		{"review@" + sha("nope"), domain.WorkflowRef{}, domain.CodeNotFound},
		{"missing", domain.WorkflowRef{}, domain.CodeNotFound},
		{"review@0", domain.WorkflowRef{}, domain.CodeInvalidRequest},
		{"review@01", domain.WorkflowRef{}, domain.CodeInvalidRequest},
		{"Review", domain.WorkflowRef{}, domain.CodeInvalidRequest},
		{"", domain.WorkflowRef{}, domain.CodeInvalidRequest},
	}
	for _, c := range cases {
		got, err := e.app.Workflows.Resolve(t.Context(), c.selector)
		if c.code != "" {
			wantCode(t, err, c.code)
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Resolve(%q) = %+v, %v; want %+v", c.selector, got, err, c.want)
		}
	}

	// Usable inside a transaction, as the creation of a Run needs.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		got, err := workflows.Resolve(t.Context(), tx, "review@1")
		if err != nil || got.Version != 1 {
			t.Errorf("in a transaction = %+v, %v", got, err)
		}
		return err
	}))
}

func TestWorkflows_Blobs(t *testing.T) {
	e := newWorkflowEnv(t, 16)
	ctx := t.Context()

	info, err := e.app.Workflows.PutBlob(ctx, e.owner, sha("hello"), strings.NewReader("hello"))
	if err != nil || !info.Created || info.Size != 5 {
		t.Fatalf("put = %+v, %v", info, err)
	}
	if again, err := e.app.Workflows.PutBlob(ctx, e.owner, sha("hello"), strings.NewReader("hello")); err != nil || again.Created || again.Size != 5 {
		t.Errorf("second put = %+v, %v", again, err)
	}
	rc, size, err := e.app.Workflows.OpenBlob(ctx, e.owner, sha("hello"))
	must(t, err)
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "hello" || size != 5 {
		t.Errorf("open = %q, %d", body, size)
	}

	big := strings.Repeat("x", 17)
	_, err = e.app.Workflows.PutBlob(ctx, e.owner, sha(big), strings.NewReader(big))
	wantCode(t, err, domain.CodeInvalidRequest)
	if _, _, err := e.app.Workflows.OpenBlob(ctx, e.owner, sha(big)); err == nil {
		t.Error("a blob over the limit can be opened")
	}
	if installed := countFiles(t, filepath.Join(e.cas, "blobs")); installed != 1 {
		t.Errorf("%d blobs installed, want 1 (no partial install)", installed)
	}

	_, err = e.app.Workflows.PutBlob(ctx, e.owner, sha("declared"), strings.NewReader("other"))
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = e.app.Workflows.PutBlob(ctx, e.owner, "not-a-digest", strings.NewReader("x"))
	wantCode(t, err, domain.CodeInvalidRequest)
	_, _, err = e.app.Workflows.OpenBlob(ctx, e.owner, sha("absent"))
	wantCode(t, err, domain.CodeNotFound)

	worker := pki.Session{Principal: domain.Principal{Kind: domain.KindWorker, ID: "worker-x"}}
	_, err = e.app.Workflows.PutBlob(ctx, worker, sha("hello"), strings.NewReader("hello"))
	wantCode(t, err, domain.CodePolicyDenied)
	_, _, err = e.app.Workflows.OpenBlob(ctx, worker, sha("hello"))
	wantCode(t, err, domain.CodePolicyDenied)
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	must(t, filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	}))
	return n
}

func TestWorkflows_SecretRefsAreThoseOfTheContent(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	with := func(content string, refs []string) domain.Manifest {
		c := e.component("codex", "cfg", content)
		c.Files[0].SecretRefs = refs
		return manifest(c)
	}
	for name, m := range map[string]domain.Manifest{
		"declared but absent":    with("plain", []string{"tok"}),
		"used but undeclared":    with("a={{cf-secret:tok}}", nil),
		"declared the wrong one": with("a={{cf-secret:tok}}", []string{"other"}),
	} {
		_, err := e.commit("review", 0, m)
		wantCode(t, err, domain.CodeInvalidRequest)
		if err != nil && !strings.Contains(err.Error(), "secret_refs") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	e.mustCommit("review", 0, with("a={{cf-secret:b}} {{cf-secret:a}} {{cf-secret:b}}", []string{"b", "a"}))
}

func TestWorkflows_ADanglingMarkerIsRefused(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	c := e.component("codex", "cfg", "a={{cf-secret:tok}} b={{cf-secret:Bad Ref}}")
	c.Files[0].SecretRefs = []string{"tok"}
	_, err := e.commit("review", 0, manifest(c))
	wantCode(t, err, domain.CodeInvalidRequest)
	if err == nil || !strings.Contains(err.Error(), "not a valid marker") {
		t.Errorf("err = %v", err)
	}
}
