package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/workflowadopt"
	"github.com/Autarquico/codefabric/internal/domain"
)

// secretValue never reaches the CLI: the test asserts it appears nowhere.
const secretValue = "ghp_0123456789abcdefghijklmnopqrstuvwx"

func component(harness, name string, own domain.Ownership, file string) domain.ManifestComponent {
	return domain.ManifestComponent{
		Harness: harness, Name: name, Kind: "settings", Ownership: own, TreeDigest: "sha256:" + strings.Repeat("a", 64),
		Files: []domain.ManifestFile{{Path: file, SHA256: "sha256:" + strings.Repeat("b", 64)}},
	}
}

func manifestOf(cs ...domain.ManifestComponent) domain.Manifest {
	return domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Components: cs}
}

func versionOf(n int, m *domain.Manifest) v1.WorkflowVersion {
	return v1.WorkflowVersion{
		Workflow: "team", WorkflowID: "wf_1", Version: n, Digest: "sha256:" + strings.Repeat("c", 64),
		Author: v1.WorkflowAuthor{Kind: "client", ID: "cli_1"}, Origin: "client", Changelog: "tune",
		Changed:   []v1.ChangedComponent{{Harness: "codex", Name: "settings", Change: "modified"}},
		CreatedAt: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), Manifest: m,
	}
}

// fakeStore answers the reads of the workflow commands; heads are what
// successive ShowWorkflow calls return (the last repeats).
type fakeStore struct {
	heads []v1.WorkflowVersion
	shows int
}

func (s *fakeStore) HasBlob(context.Context, string) (bool, error) { return true, nil }
func (s *fakeStore) PutBlob(context.Context, string, io.Reader, int64) (v1.BlobMeta, error) {
	return v1.BlobMeta{}, nil
}

func (s *fakeStore) CommitWorkflow(context.Context, string, v1.CommitWorkflowRequest) (v1.WorkflowVersion, error) {
	return v1.WorkflowVersion{}, errors.New("the fake adopter commits")
}

func (s *fakeStore) ShowWorkflow(context.Context, string) (v1.WorkflowVersion, error) {
	if len(s.heads) == 0 {
		return v1.WorkflowVersion{}, domain.NewError(domain.CodeNotFound, "no such workflow", nil)
	}
	i := min(s.shows, len(s.heads)-1)
	s.shows++
	return s.heads[i], nil
}

func (s *fakeStore) ListWorkflows(context.Context) ([]v1.WorkflowSummary, error) {
	return []v1.WorkflowSummary{{Name: "team", ID: "wf_1", Head: 2, HeadDigest: "sha256:" + strings.Repeat("c", 64)}, {Name: "empty", ID: "wf_2"}}, nil
}

func (s *fakeStore) WorkflowHistory(context.Context, string) ([]v1.WorkflowVersion, error) {
	return []v1.WorkflowVersion{versionOf(2, nil), versionOf(1, nil)}, nil
}

// fakeAdopter snapshots what it is told; it behaves as the real Adopter does
// about secrets: no confirmer aborts, a declining one aborts.
type fakeAdopter struct {
	snapshot  domain.Manifest
	secrets   []workflowadopt.HarnessSecrets
	confirmer workflowadopt.Confirmer
	// commitErrs are the errors of successive Commits; then it succeeds.
	commitErrs []error
	selections [][]workflowadopt.Selection
	commits    []commitCall
}

type commitCall struct {
	name      string
	parent    int
	changelog string
	manifest  domain.Manifest
}

func (a *fakeAdopter) Harnesses() []string { return []string{"codex"} }
func (a *fakeAdopter) Components(context.Context, string) ([]WorkflowComponent, error) {
	return []WorkflowComponent{{Name: "settings", Kind: "settings"}}, nil
}

func (a *fakeAdopter) Prepare(ctx context.Context, sel []workflowadopt.Selection) (*workflowadopt.Prepared, error) {
	a.selections = append(a.selections, sel)
	if len(a.secrets) > 0 {
		ok, err := a.confirmer.ConfirmSecrets(ctx, a.secrets)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: declined", workflowadopt.ErrSecretsUnconfirmed)
		}
	}
	m := a.snapshot
	m.Components = append([]domain.ManifestComponent(nil), m.Components...)
	return &workflowadopt.Prepared{Manifest: m}, nil
}

func (a *fakeAdopter) Commit(_ context.Context, name string, p *workflowadopt.Prepared, parent int, changelog string) (v1.WorkflowVersion, error) {
	a.commits = append(a.commits, commitCall{name, parent, changelog, p.Manifest})
	if len(a.commitErrs) > 0 {
		err := a.commitErrs[0]
		a.commitErrs = a.commitErrs[1:]
		if err != nil {
			return v1.WorkflowVersion{}, err
		}
	}
	return versionOf(parent+1, nil), nil
}

func conflict() error {
	return domain.NewError(domain.CodeWorkflowConflict, "head moved", map[string]any{"head": 3})
}

func runWorkflow(t *testing.T, store *fakeStore, a *fakeAdopter, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	opts := Options{
		workflowStore: func(*cobra.Command) (workflowStore, error) { return store, nil },
		Workflows: func(_ workflowadopt.Store, c workflowadopt.Confirmer) WorkflowAdopter {
			a.confirmer = c
			return a
		},
	}
	var out, errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, args, strings.NewReader(""), &out, &errOut)
	for _, s := range []string{out.String(), errOut.String()} {
		if strings.Contains(s, secretValue) {
			t.Fatalf("a secret value reached the output: %q", s)
		}
	}
	return code, out.String(), errOut.String()
}

func headManifest() domain.Manifest {
	return manifestOf(component("codex", "settings", domain.OwnershipManaged, "config.toml"))
}

func TestWorkflowReadCommands(t *testing.T) {
	hm := headManifest()
	store := &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm)}}
	a := &fakeAdopter{}

	code, out, _ := runWorkflow(t, store, a, "workflow", "list", "--json")
	var list v1.ListWorkflowsResponse
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 || len(list.Workflows) != 2 || list.Workflows[0].Head != 2 {
		t.Fatalf("list --json: exit %d, %q, %v", code, out, err)
	}
	if _, out, _ = runWorkflow(t, store, a, "workflow", "list"); !strings.Contains(out, "v2") || !strings.Contains(out, "empty") {
		t.Errorf("list: %q", out)
	}

	code, out, _ = runWorkflow(t, store, a, "workflow", "show", "team@2", "--json")
	var v v1.WorkflowVersion
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || v.Manifest == nil || v.Author.ID != "cli_1" {
		t.Fatalf("show --json: exit %d, %q, %v", code, out, err)
	}
	_, out, _ = runWorkflow(t, store, a, "workflow", "show", "team")
	for _, want := range []string{"team v2", "client:cli_1", "tune", "modified codex/settings", "managed", "sha256:aaaaaaaaaaaa"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q: %s", want, out)
		}
	}

	code, out, _ = runWorkflow(t, store, a, "workflow", "history", "team", "--json")
	var h v1.WorkflowHistoryResponse
	if err := json.Unmarshal([]byte(out), &h); err != nil || code != 0 || len(h.Versions) != 2 {
		t.Fatalf("history --json: exit %d, %q, %v", code, out, err)
	}
	if _, out, _ = runWorkflow(t, store, a, "workflow", "history", "team"); !strings.Contains(out, "v2") || !strings.Contains(out, "tune") || !strings.Contains(out, "client") {
		t.Errorf("history: %q", out)
	}

	if code, _, _ = runWorkflow(t, store, a, "workflow", "show"); code != 2 {
		t.Errorf("show without a ref: exit %d", code)
	}
}

func TestAdoptHeadlessNeedsItsFlags(t *testing.T) {
	a := &fakeAdopter{snapshot: headManifest()}
	store := &fakeStore{}
	for name, args := range map[string][]string{
		"no --yes":      {"workflow", "adopt", "team", "--harness", "codex"},
		"no --harness":  {"workflow", "adopt", "team", "--yes"},
		"no name":       {"workflow", "adopt", "--yes", "--harness", "codex"},
		"unknown local": {"workflow", "adopt", "team", "--yes", "--harness", "codex", "--local", "nope"},
	} {
		code, out, _ := runWorkflow(t, store, a, append(args, "--json")...)
		if env := errorOf(t, out); code != 2 || env["code"] != string(domain.CodeInvalidRequest) {
			t.Errorf("%s: exit %d, %v", name, code, env)
		}
	}
	if len(a.commits) != 0 {
		t.Errorf("committed anyway: %+v", a.commits)
	}
}

func TestAdoptHeadlessRefusesSecretsWithoutReplaceSecrets(t *testing.T) {
	secrets := []workflowadopt.HarnessSecrets{{Harness: "codex", Report: workflowadopt.Report{
		Substituted: []workflowadopt.Substituted{{Component: "settings", File: "config.toml", Ref: "github-token"}},
		Hits:        []workflowadopt.Hit{{Component: "settings", File: "config.toml", Line: 3, Kind: "github-token", Preview: "ghp_… (40)", Ref: "codex-settings-token", Replaceable: true}},
	}}}
	a := &fakeAdopter{snapshot: headManifest(), secrets: secrets}
	args := []string{"workflow", "adopt", "team", "--harness", "codex", "--yes"}

	code, out, _ := runWorkflow(t, &fakeStore{}, a, append(args, "--json")...)
	if env := errorOf(t, out); code != 2 || env["code"] != string(domain.CodeInvalidRequest) || len(a.commits) != 0 {
		t.Fatalf("without --replace-secrets: exit %d, %v, commits %d", code, env, len(a.commits))
	}

	code, out, _ = runWorkflow(t, &fakeStore{}, a, append(args, "--replace-secrets", "--changelog", "first", "--json")...)
	var v v1.WorkflowVersion
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || v.Version != 1 {
		t.Fatalf("with --replace-secrets: exit %d, %q, %v", code, out, err)
	}
	if len(a.commits) != 1 || a.commits[0].parent != 0 || a.commits[0].changelog != "first" || a.commits[0].name != "team" {
		t.Errorf("commits: %+v", a.commits)
	}
}

func TestAdoptMarksComponentsLocalAndSelectsComponents(t *testing.T) {
	a := &fakeAdopter{snapshot: manifestOf(
		component("codex", "settings", domain.OwnershipManaged, "config.toml"),
		component("codex", "skills", domain.OwnershipManaged, "skills/a.md"),
	)}
	code, out, errOut := runWorkflow(t, &fakeStore{}, a, "workflow", "adopt", "team", "--harness", "codex", "--component", "settings", "--local", "codex/skills", "--yes")
	if code != 0 || !strings.Contains(out, "team is at v1") {
		t.Fatalf("exit %d: %q %q", code, out, errOut)
	}
	if got := a.selections[0]; len(got) != 1 || got[0].Harness != "codex" || strings.Join(got[0].Components, ",") != "settings" {
		t.Errorf("selection: %+v", got)
	}
	own := map[string]domain.Ownership{}
	for _, c := range a.commits[0].manifest.Components {
		own[c.Name] = c.Ownership
	}
	if own["skills"] != domain.OwnershipLocal || own["settings"] != domain.OwnershipManaged {
		t.Errorf("ownership: %v", own)
	}
}

func TestWorkflowAdoptWithoutAdaptersIsNotImplemented(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := Options{workflowStore: func(*cobra.Command) (workflowStore, error) { return &fakeStore{}, nil }}
	code := RunContextWith(context.Background(), opts, []string{"workflow", "adopt", "x", "--harness", "codex", "--yes", "--json"}, strings.NewReader(""), &out, &errOut)
	if env := errorOf(t, out.String()); code != 8 || env["code"] != string(domain.CodeNotImplemented) {
		t.Errorf("exit %d, %v", code, env)
	}
}

func TestCommitShowsTheDiffAndCommitsOnTheHead(t *testing.T) {
	hm := headManifest()
	next := manifestOf(component("codex", "settings", domain.OwnershipManaged, "other.toml"))
	store := &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm)}}
	a := &fakeAdopter{snapshot: next}

	code, out, errOut := runWorkflow(t, store, a, "workflow", "commit", "team", "--yes", "--json")
	var v v1.WorkflowVersion
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || v.Version != 3 {
		t.Fatalf("exit %d: %q %q %v", code, out, errOut, err)
	}
	if !strings.Contains(errOut, "modified") || !strings.Contains(errOut, "codex/settings") {
		t.Errorf("no diff shown: %q", errOut)
	}
	if a.commits[0].parent != 2 {
		t.Errorf("parent %d, want the head (2)", a.commits[0].parent)
	}
	if got := a.selections[0]; len(got) != 1 || got[0].Harness != "codex" || strings.Join(got[0].Components, ",") != "settings" {
		t.Errorf("default selection is not the head's: %+v", got)
	}
}

func TestCommitKeepsTheOwnershipOfTheHead(t *testing.T) {
	hm := manifestOf(component("codex", "settings", domain.OwnershipLocal, "config.toml"))
	a := &fakeAdopter{snapshot: manifestOf(component("codex", "settings", domain.OwnershipManaged, "other.toml"))}
	if code, _, e := runWorkflow(t, &fakeStore{heads: []v1.WorkflowVersion{versionOf(1, &hm)}}, a, "workflow", "commit", "team", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	if got := a.commits[0].manifest.Components[0].Ownership; got != domain.OwnershipLocal {
		t.Errorf("ownership %q, want local", got)
	}
}

func TestCommitKeepsWhatTheHeadKeptLocal(t *testing.T) {
	hm := manifestOf(component("codex", "settings", domain.OwnershipLocal, "config.toml"))
	a := &fakeAdopter{snapshot: manifestOf(component("codex", "settings", domain.OwnershipManaged, "changed.toml"))}
	code, _, errOut := runWorkflow(t, &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm)}}, a, "workflow", "commit", "team", "--yes")
	if code != 0 || len(a.commits) != 1 || a.commits[0].manifest.Components[0].Ownership != domain.OwnershipLocal {
		t.Errorf("exit %d, commits %+v, %q", code, a.commits, errOut)
	}
}

func TestCommitWithNothingNewDoesNotCommit(t *testing.T) {
	hm := headManifest()
	a := &fakeAdopter{snapshot: headManifest()}
	code, _, errOut := runWorkflow(t, &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm)}}, a, "workflow", "commit", "team", "--yes")
	if code != 0 || len(a.commits) != 0 || !strings.Contains(errOut, "No changes") {
		t.Errorf("exit %d, commits %d, %q", code, len(a.commits), errOut)
	}
}

func TestCommitConflictExits4(t *testing.T) {
	hm := headManifest()
	a := &fakeAdopter{snapshot: manifestOf(component("codex", "settings", domain.OwnershipManaged, "x")), commitErrs: []error{conflict()}}
	code, out, _ := runWorkflow(t, &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm)}}, a, "workflow", "commit", "team", "--yes", "--json")
	if env := errorOf(t, out); code != 4 || env["code"] != string(domain.CodeWorkflowConflict) {
		t.Errorf("exit %d, %v", code, env)
	}
	if len(a.commits) != 1 {
		t.Errorf("commits without --rebase: %d", len(a.commits))
	}
}

func TestCommitRebaseRereadsTheHeadAndCommitsOnIt(t *testing.T) {
	hm2, hm3 := headManifest(), manifestOf(component("codex", "settings", domain.OwnershipManaged, "moved.toml"))
	store := &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm2), versionOf(3, &hm3)}}
	a := &fakeAdopter{snapshot: manifestOf(component("codex", "settings", domain.OwnershipManaged, "mine.toml")), commitErrs: []error{conflict()}}
	code, out, errOut := runWorkflow(t, store, a, "workflow", "commit", "team", "--yes", "--rebase", "--json")
	var v v1.WorkflowVersion
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || v.Version != 4 {
		t.Fatalf("exit %d: %q %q %v", code, out, errOut, err)
	}
	if len(a.commits) != 2 || a.commits[0].parent != 2 || a.commits[1].parent != 3 || len(a.selections) != 2 {
		t.Errorf("commits %+v, snapshots %d: want a re-snapshot on the new head", a.commits, len(a.selections))
	}
}

func TestCommitRebaseSecondConflictIsFinal(t *testing.T) {
	hm := headManifest()
	store := &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm), versionOf(3, &hm), versionOf(4, &hm)}}
	a := &fakeAdopter{snapshot: manifestOf(component("codex", "settings", domain.OwnershipManaged, "mine.toml")), commitErrs: []error{conflict(), conflict()}}
	code, out, _ := runWorkflow(t, store, a, "workflow", "commit", "team", "--yes", "--rebase", "--json")
	if env := errorOf(t, out); code != 4 || env["code"] != string(domain.CodeWorkflowConflict) || len(a.commits) != 2 {
		t.Errorf("exit %d, %v, commits %d", code, env, len(a.commits))
	}
}

func TestCommitHeadlessNeedsYes(t *testing.T) {
	hm := headManifest()
	a := &fakeAdopter{snapshot: headManifest()}
	for _, args := range [][]string{{"workflow", "commit", "team"}, {"workflow", "commit", "team", "--rebase"}} {
		if code, _, _ := runWorkflow(t, &fakeStore{heads: []v1.WorkflowVersion{versionOf(2, &hm)}}, a, args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func newPrompter(answers string) (*prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return &prompter{in: bufio.NewReader(strings.NewReader(answers)), out: &out}, &out
}

func TestPickAndConfirm(t *testing.T) {
	items := []string{"a", "b", "c"}
	for _, tc := range []struct {
		answers string
		def     []int
		want    []int
	}{
		{"\n", nil, []int{0, 1, 2}},
		{"\n", []int{}, []int{}},
		{"2,3\n", nil, []int{1, 2}},
		{"3, 1, 3\n", nil, []int{0, 2}},
		{"none\n", nil, []int{}},
		{"ALL\n", []int{}, []int{0, 1, 2}},
		{"9\n2\n", nil, []int{1}}, // an invalid answer is asked again
	} {
		p, _ := newPrompter(tc.answers)
		got, err := p.pick("Things", items, tc.def)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%q: %v, %v; want %v", tc.answers, got, err, tc.want)
		}
	}
	p, _ := newPrompter("")
	if _, err := p.pick("Things", items, nil); err == nil {
		t.Error("closed input: no error")
	}
	for answer, want := range map[string]bool{"y\n": true, "YES\n": true, "\n": false, "n\n": false, "maybe\n": false} {
		p, _ := newPrompter(answer)
		if got, err := p.confirm("Sure?"); err != nil || got != want {
			t.Errorf("%q: %v, %v", answer, got, err)
		}
	}
}

func TestConfirmerListsEveryRefAndNeverAValue(t *testing.T) {
	found := []workflowadopt.HarnessSecrets{{Harness: "codex", Report: workflowadopt.Report{
		Substituted: []workflowadopt.Substituted{{Component: "settings", File: "config.toml", Ref: "github-token"}},
		Hits:        []workflowadopt.Hit{{Component: "settings", File: "config.toml", Line: 7, Kind: "github-token", Preview: "ghp_… (40 chars)", Ref: "codex-settings-token", Replaceable: true}},
	}}}
	p, out := newPrompter("y\n")
	ok, err := cliConfirmer{ui: p}.ConfirmSecrets(context.Background(), found)
	if err != nil || !ok {
		t.Fatalf("accepted: %v, %v", ok, err)
	}
	for _, want := range []string{"github-token", "config.toml:7", "ghp_… (40 chars)", "codex-settings-token"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the list lacks %q: %s", want, out)
		}
	}
	p, _ = newPrompter("n\n")
	if ok, err := (cliConfirmer{ui: p}).ConfirmSecrets(context.Background(), found); err != nil || ok {
		t.Errorf("declined: %v, %v", ok, err)
	}
}
