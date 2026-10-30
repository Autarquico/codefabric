package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

type fakeEnvironments struct {
	applied []v1.ApplyEnvironmentRequest
	refuse  error // what ApplyEnvironment answers instead of a version
}

func envVersion(n int) v1.EnvironmentVersion {
	return v1.EnvironmentVersion{
		Environment: "node", Version: n, Digest: "sha256:" + strings.Repeat("d", 64),
		Author: v1.EnvironmentAuthor{Kind: "client", ID: "cli_1"}, CreatedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
	}
}

func (f *fakeEnvironments) ListEnvironments(context.Context) ([]v1.EnvironmentSummary, error) {
	return []v1.EnvironmentSummary{{Name: "node", Head: 2, HeadDigest: "sha256:" + strings.Repeat("d", 64)}, {Name: "empty"}}, nil
}

func (f *fakeEnvironments) ShowEnvironment(context.Context, string) (v1.ShowEnvironmentResponse, error) {
	head := envVersion(2)
	head.Definition = &domain.EnvironmentDef{Provider: "host", Prepare: "npm ci", Services: []domain.EnvironmentService{{Name: "db", Start: "pg start"}}}
	return v1.ShowEnvironmentResponse{EnvironmentVersion: head, Versions: []v1.EnvironmentVersion{envVersion(2), envVersion(1)}}, nil
}

func (f *fakeEnvironments) ApplyEnvironment(_ context.Context, _ string, req v1.ApplyEnvironmentRequest) (v1.EnvironmentVersion, error) {
	f.applied = append(f.applied, req)
	if f.refuse != nil {
		return v1.EnvironmentVersion{}, f.refuse
	}
	return envVersion(3), nil
}

func runEnvironment(t *testing.T, f *fakeEnvironments, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	opts := Options{environmentStore: func(*cobra.Command) (environmentStore, error) { return f, nil }}
	var out, errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func writeDefinition(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDecodeEnvironmentFile_IsStrict(t *testing.T) {
	good, err := decodeEnvironmentFile([]byte(`{"name":"node","provider":"host","prepare":"npm ci","digest_inputs":["package-lock.json"]}`))
	if err != nil || good.Name != "node" || good.Provider != "host" || good.Prepare != "npm ci" || len(good.DigestInputs) != 1 {
		t.Fatalf("good = %+v, %v", good, err)
	}
	for name, body := range map[string]string{
		"unknown field": `{"provider":"host","prepar":"x"}`,
		"two documents": `{"provider":"host"} {"provider":"host"}`,
		"not an object": `[]`,
		"empty":         ``,
	} {
		if _, err := decodeEnvironmentFile([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEnvironmentApply_SendsTheDefinitionUnderTheNameOfTheFile(t *testing.T) {
	f := &fakeEnvironments{}
	path := writeDefinition(t, `{"name":"node","provider":"host","prepare":"npm ci"}`)
	code, out, _ := runEnvironment(t, f, "environment", "apply", "-f", path)
	if code != 0 || !strings.Contains(out, "node is at v3") || len(f.applied) != 1 || f.applied[0].Definition.Prepare != "npm ci" {
		t.Fatalf("apply: exit %d, %q, %+v", code, out, f.applied)
	}
}

func TestEnvironmentApply_NeedsOneName(t *testing.T) {
	f := &fakeEnvironments{}
	if code, _, _ := runEnvironment(t, f, "environment", "apply", "-f", writeDefinition(t, `{"provider":"host"}`)); code != 2 {
		t.Errorf("no name: exit %d, want 2", code)
	}
	if code, _, _ := runEnvironment(t, f, "environment", "apply", "-f", writeDefinition(t, `{"name":"a","provider":"host"}`), "--name", "b"); code != 2 {
		t.Errorf("two names: exit %d, want 2", code)
	}
	if code, _, _ := runEnvironment(t, f, "environment", "apply", "-f", writeDefinition(t, `{"provider":"host"}`), "--name", "node"); code != 0 {
		t.Errorf("--name: exit %d, want 0", code)
	}
	if len(f.applied) != 1 {
		t.Errorf("applied %d times, want only the valid one", len(f.applied))
	}
}

func TestEnvironmentApply_NothingToCommitIsNoChangesAndExitZero(t *testing.T) {
	f := &fakeEnvironments{refuse: domain.NewError(domain.CodeInvalidRequest, "nothing to commit", map[string]any{"nothing_to_commit": true, "head": 2})}
	path := writeDefinition(t, `{"name":"node","provider":"host"}`)
	code, out, errOut := runEnvironment(t, f, "environment", "apply", "-f", path)
	if code != 0 || !strings.Contains(errOut, "No changes against node v2") || !strings.Contains(out, "node is at v2") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	f.refuse = domain.NewError(domain.CodeInvalidRequest, "provider is not supported", nil)
	if code, _, _ := runEnvironment(t, f, "environment", "apply", "-f", path); code == 0 {
		t.Error("another invalid request exited 0")
	}
}

func TestEnvironmentReadCommands(t *testing.T) {
	f := &fakeEnvironments{}
	code, out, _ := runEnvironment(t, f, "environment", "list", "--json")
	var list v1.ListEnvironmentsResponse
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 || len(list.Environments) != 2 {
		t.Fatalf("list --json: exit %d, %q, %v", code, out, err)
	}
	if _, out, _ = runEnvironment(t, f, "environment", "list"); !strings.Contains(out, "v2") || !strings.Contains(out, "empty") {
		t.Errorf("list: %q", out)
	}
	_, out, _ = runEnvironment(t, f, "environment", "show", "node")
	for _, want := range []string{"node v2", "npm ci", "db: start pg start", "sha256:dddddddddddd", "v1"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q: %s", want, out)
		}
	}
	if code, _, _ := runEnvironment(t, f, "environment", "show"); code != 2 {
		t.Errorf("show without a ref: exit %d", code)
	}
}
