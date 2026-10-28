package codex_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// plainSecrets are the secret values of testdata/home/.codex/config.toml and
// auth.json.
var plainSecrets = []string{
	"sk-docs-secret-1", "web-secret-2", "odd-secret-3", "corp-secret-4", "corp-secret-5", "gh-secret-6",
	"auth-secret-7", "auth-secret-8",
}

// homeWith is a Host whose home is a copy of testdata/home; extra replaces
// the files it names (relative to ~/.codex).
func homeWith(t *testing.T, extra map[string]string) harness.Host {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join("testdata", "home")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(home, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := os.ReadFile(p) //nolint:gosec // testdata
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, info.Mode().Perm()) //nolint:gosec // temporary home
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range extra {
		p := filepath.Join(home, ".codex", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func snapshot(t *testing.T, h harness.Host, components ...string) harness.NativeSnapshot {
	t.Helper()
	s, err := codex.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: components})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func names(s harness.NativeSnapshot) []string {
	var out []string
	for _, c := range s.Components {
		out = append(out, c.Name)
	}
	return out
}

func file(t *testing.T, s harness.NativeSnapshot, name string) harness.File {
	t.Helper()
	for _, c := range s.Components {
		if c.Name == name {
			return c.Files[0]
		}
	}
	t.Fatalf("no component %s in %v", name, names(s))
	return harness.File{}
}

func codexDir(h harness.Host) string { return filepath.Join(h.Home(), ".codex") }

func TestScenario_Snapshot(t *testing.T) {
	h := homeWith(t, nil)
	s := snapshot(t, h)
	if want := []string{"config.toml", "AGENTS.md", "prompts", "skills"}; !slices.Equal(names(s), want) {
		t.Fatalf("components = %v, want %v", names(s), want)
	}
	for _, c := range s.Components {
		want, err := harness.FilesDigest(c.Files)
		if err != nil || c.Digest != want || c.Ownership != harness.Managed {
			t.Errorf("%s: digest %s (want %s, %v), ownership %s", c.Name, c.Digest, want, err, c.Ownership)
		}
		for _, f := range c.Files {
			if err := f.CheckSecrets(); err != nil {
				t.Error(err)
			}
			if f.Path == "auth.json" || strings.Contains(f.Path, "history") {
				t.Errorf("the snapshot has %s", f.Path)
			}
			for _, secret := range plainSecrets {
				if bytes.Contains(f.Content, []byte(secret)) {
					t.Errorf("%s carries the secret %q", f.Path, secret)
				}
			}
		}
	}
	cfg := file(t, s, "config.toml")
	wantRefs := []string{
		"codex/mcp/docs/env/DOCS_API_KEY",
		"codex/mcp/docs/env/REGION",
		"codex/mcp/web/http_headers/Authorization",
		"codex/model_providers/corp/experimental_bearer_token",
		"codex/model_providers/corp/query_params/api-version",
		"codex/shell_environment_policy/set/GH_TOKEN",
	}
	odd := regexp.MustCompile(`^codex/mcp/web/http_headers/X_Odd_Key-[0-9a-f]{8}$`)
	var got []string
	for _, ref := range cfg.SecretRefs {
		if odd.MatchString(ref) {
			got = append(got, "odd")
			continue
		}
		got = append(got, ref)
		if !domain.ValidSecretRef(ref) {
			t.Errorf("invalid ref %q", ref)
		}
	}
	slices.Sort(got)
	if want := append(slices.Clone(wantRefs), "odd"); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("refs = %q, want %q", got, want)
	}
	text := string(cfg.Content)
	for _, keep := range []string{`model = 'gpt-5'`, `DOCS_HOME`, `DOCS_TOKEN`, `ORG_HEADER_VAR`, `CORP_API_KEY`} {
		if !strings.Contains(text, keep) {
			t.Errorf("config.toml lost %s:\n%s", keep, text)
		}
	}
	if strings.Contains(text, "projects") || strings.Contains(text, "trusted") {
		t.Errorf("config.toml keeps the trust of this machine:\n%s", text)
	}
}

func TestSnapshot_SelectsComponentsAndSkipsAbsentOnes(t *testing.T) {
	h := homeWith(t, nil)
	if got := names(snapshot(t, h, "skills", "AGENTS.md")); !slices.Equal(got, []string{"AGENTS.md", "skills"}) {
		t.Errorf("selected = %v", got)
	}
	if err := os.Remove(filepath.Join(codexDir(h), "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if got := names(snapshot(t, h)); slices.Contains(got, "AGENTS.md") {
		t.Errorf("absent AGENTS.md is in %v", got)
	}
	if _, err := (codex.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: []string{"auth.json"}}); err == nil {
		t.Error("auth.json can be asked for as a component")
	}
}

func TestSnapshot_IsDeterministicAndStableUnderSecretRotation(t *testing.T) {
	a := snapshot(t, homeWith(t, nil))
	b := snapshot(t, homeWith(t, nil))
	if !bytes.Equal(file(t, a, "config.toml").Content, file(t, b, "config.toml").Content) {
		t.Error("the same config.toml gave different bytes")
	}
	rotated := snapshot(t, homeWith(t, map[string]string{"auth.json": `{"tokens":{"access_token":"other"}}`}))
	if a.Components[0].Digest != rotated.Components[0].Digest {
		t.Error("rotating auth.json changed the snapshot")
	}
	// the text is the serialiser's: a snapshot of the snapshot changes nothing
	cfg := file(t, a, "config.toml").Content
	again := file(t, snapshot(t, homeWith(t, map[string]string{"config.toml": string(cfg)})), "config.toml").Content
	if !bytes.Equal(again, cfg) {
		t.Errorf("the snapshot is not a fixed point of the serialiser:\n%s\nthen\n%s", cfg, again)
	}
}

func TestSnapshot_InvalidTOMLIsAnError(t *testing.T) {
	h := homeWith(t, map[string]string{"config.toml": "model = \n"})
	if _, err := (codex.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil {
		t.Error("an invalid config.toml was snapshotted")
	}
}

func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	d, err := treedigest.Dir(dir, treedigest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestScenario_ApplyAislado(t *testing.T) {
	h := homeWith(t, nil)
	s := snapshot(t, h)
	before := dirDigest(t, h.Home())
	run := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := codex.Adapter{}.ApplyConfig(t.Context(), h, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: run}, LinkAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if after := dirDigest(t, h.Home()); after != before {
		t.Errorf("~/.codex changed: %s then %s", before, after)
	}
	if !slices.Equal(res.AuthCopies, []string{"auth.json"}) || len(res.AuthLinks) != 0 {
		t.Fatalf("AuthCopies = %v, AuthLinks = %v", res.AuthCopies, res.AuthLinks)
	}
	want, _ := os.ReadFile(filepath.Join(codexDir(h), "auth.json")) //nolint:gosec // temporary dir
	copied := filepath.Join(run, "auth.json")
	if fi, err := os.Lstat(copied); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("auth.json of the Run is not a 0600 regular file (%v)", err)
	}
	if got, _ := os.ReadFile(copied); !bytes.Equal(got, want) || len(want) == 0 { //nolint:gosec // temporary dir
		t.Error("auth.json of the Run is not a copy of the host's")
	}
	got, err := os.ReadFile(filepath.Join(run, "config.toml")) //nolint:gosec // temporary dir
	if err != nil || !bytes.Equal(got, file(t, s, "config.toml").Content) {
		t.Errorf("config.toml of the Run is not the snapshot's (%v)", err)
	}
	if fi, err := os.Stat(filepath.Join(run, "skills", "review", "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh lost its mode (%v)", err)
	}
	d1, err := treedigest.Dir(run, treedigest.Options{ApprovedFiles: res.AuthCopies})
	if err != nil {
		t.Fatal(err)
	}
	run2 := t.TempDir()
	if _, err := (codex.Adapter{}).ApplyConfig(t.Context(), h, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: run2}, LinkAuth: true}); err != nil {
		t.Fatal(err)
	}
	if d2, _ := treedigest.Dir(run2, treedigest.Options{ApprovedFiles: res.AuthCopies}); d2 != d1 {
		t.Error("the same snapshot gave two digests")
	}
}

func TestApplyRun_DryRunAndNoLinkWriteNothing(t *testing.T) {
	h := homeWith(t, nil)
	s := snapshot(t, h)
	dir := t.TempDir()
	res, err := codex.Adapter{}.ApplyConfig(t.Context(), h, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true, DryRun: true})
	if err != nil || len(res.Changes) != 4 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dry run wrote %v", entries)
	}
	res, err = codex.Adapter{}.ApplyConfig(t.Context(), h, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}})
	if err != nil || len(res.AuthLinks) != 0 || len(res.AuthCopies) != 0 {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "auth.json")); err == nil {
		t.Error("auth.json given without LinkAuth")
	}
	if _, err := (codex.Adapter{}).ApplyConfig(t.Context(), h, s, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}}); err == nil {
		t.Error("applied to a directory that is not empty")
	}
}

func TestApplyGlobal_RejectsMarkersAndWritesNothing(t *testing.T) {
	h := homeWith(t, nil)
	s := snapshot(t, h)
	before := dirDigest(t, h.Home())
	for _, dry := range []bool{true, false} {
		_, err := codex.Adapter{}.ApplyConfig(t.Context(), h, s, harness.ApplyOptions{Target: harness.TargetGlobal{}, DryRun: dry})
		if !errors.Is(err, harness.ErrSecretUnavailable) {
			t.Errorf("dry=%v: err = %v", dry, err)
		}
	}
	if dirDigest(t, h.Home()) != before {
		t.Error("a rejected apply wrote")
	}
}

func TestApplyGlobal_KeepsLocalSettingsAndComponents(t *testing.T) {
	src := homeWith(t, map[string]string{"config.toml": "model = \"gpt-5\"\n[mcp_servers.docs]\ncommand = \"npx\"\n"})
	s := snapshot(t, src)
	dst := homeWith(t, map[string]string{
		"config.toml":       "model = \"old\"\ntheme = \"dark\"\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n[mcp_servers.mine]\ncommand = \"mine\"\n",
		"AGENTS.md":         "mine",
		"prompts/local.md":  "mine",
		"skills/stale/a.md": "stale",
	})
	// ceded: the person owns AGENTS.md and prompts
	for i, c := range s.Components {
		if c.Name == "prompts" || c.Name == "AGENTS.md" {
			s.Components[i].Ownership = harness.Local
		}
	}
	res, err := codex.Adapter{}.ApplyConfig(t.Context(), dst, s, harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range res.Changes {
		if ch.Name == "prompts" || ch.Name == "AGENTS.md" {
			t.Errorf("reported a change of local %s", ch.Name)
		}
	}
	b, _ := os.ReadFile(filepath.Join(codexDir(dst), "config.toml")) //nolint:gosec // temporary home
	var got map[string]any
	if err := toml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "gpt-5" || got["theme"] != "dark" || got["projects"] == nil {
		t.Errorf("config.toml = %s", b)
	}
	servers := got["mcp_servers"].(map[string]any)
	if servers["mine"] == nil || servers["docs"] == nil {
		t.Errorf("mcp_servers = %v", servers)
	}
	if b, _ := os.ReadFile(filepath.Join(codexDir(dst), "prompts", "local.md")); string(b) != "mine" { //nolint:gosec // temporary home
		t.Error("a local prompt was touched")
	}
	if _, err := os.Stat(filepath.Join(codexDir(dst), "skills", "stale")); err == nil {
		t.Error("a managed skills directory was merged, not replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(codexDir(dst), "AGENTS.md")); string(b) != "mine" { //nolint:gosec // temporary home
		t.Error("local AGENTS.md was overwritten")
	}
	// local components are compared like the rest: only they differ
	if d, err := (codex.Adapter{}).DiffConfig(t.Context(), dst, s); err != nil || len(d.Changes) != 2 {
		t.Errorf("diff after apply = %+v, %v", d, err)
	}
	// an empty snapshot removes the managed settings and skills, never the local ones
	if _, err := (codex.Adapter{}).ApplyConfig(t.Context(), dst, harness.NativeSnapshot{Harness: "codex"}, harness.ApplyOptions{Target: harness.TargetGlobal{}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(codexDir(dst), "config.toml")) //nolint:gosec // temporary home
	got = nil
	if err := toml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != nil || got["theme"] != "dark" || got["projects"] == nil || got["mcp_servers"].(map[string]any)["docs"] != nil || got["mcp_servers"].(map[string]any)["mine"] == nil {
		t.Errorf("after the empty snapshot config.toml = %s", b)
	}
	if _, err := os.Stat(filepath.Join(codexDir(dst), "skills")); err == nil {
		t.Error("the managed skills survived an empty snapshot")
	}
	if _, err := os.Stat(filepath.Join(codexDir(dst), "prompts", "local.md")); err != nil {
		t.Error("a local prompt was removed")
	}
}

func TestDiffConfig_PlainSecretsMatchTheirMarkersAndEditsShow(t *testing.T) {
	h := homeWith(t, nil)
	s := snapshot(t, h)
	d, err := codex.Adapter{}.DiffConfig(t.Context(), h, s)
	if err != nil || len(d.Changes) != 0 {
		t.Fatalf("diff right after snapshot = %+v, %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(codexDir(h), "AGENTS.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(codexDir(h), "prompts")); err != nil {
		t.Fatal(err)
	}
	d, err = codex.Adapter{}.DiffConfig(t.Context(), h, s)
	want := []harness.Change{{Name: "AGENTS.md", Kind: harness.Modified}, {Name: "prompts", Kind: harness.Added}}
	if err != nil || !slices.Equal(d.Changes, want) {
		t.Errorf("diff = %+v, %v; want %v", d.Changes, err, want)
	}
}

func TestValidateConfig(t *testing.T) {
	v, err := codex.Adapter{}.ValidateConfig(t.Context(), homeWith(t, nil))
	if err != nil || len(v.Issues) != 0 {
		t.Errorf("valid home: %+v, %v", v, err)
	}
	v, err = codex.Adapter{}.ValidateConfig(t.Context(), homeWith(t, map[string]string{"config.toml": "a = = 1"}))
	if err != nil || len(v.Issues) != 1 || v.Issues[0].Component != "config.toml" {
		t.Errorf("invalid TOML: %+v, %v", v, err)
	}
}

// secretRoutesConfig has a secret in every route of config.schema.json (codex
// 0.160.1) that is not an MCP server or a model provider; the value of each is
// rotated by the placeholder.
const secretRoutesConfig = `
[otel.exporter.otlp-http]
endpoint = "https://otel.example.com"
protocol = "binary"
[otel.exporter.otlp-http.headers]
x-api-key = "%[1]s-http"

[otel.trace_exporter.otlp-grpc]
endpoint = "https://otel.example.com"
[otel.trace_exporter.otlp-grpc.headers]
authorization = "%[1]s-grpc-trace"

[otel.metrics_exporter.otlp-http]
endpoint = "https://otel.example.com"
protocol = "json"
[otel.metrics_exporter.otlp-http.headers]
x-api-key = "%[1]s-http-metrics"

[permissions.work.network.mitm.hooks.github]
host = "api.github.com"
methods = ["GET"]
path_prefixes = ["/"]
action = ["inject"]
[permissions.work.network.mitm.hooks.github.headers]
x-token = ["%[1]s-hook-a", "%[1]s-hook-b"]
`

var secretRoutes = map[string]string{
	"otel.exporter":         "codex/otel/exporter/otlp-http/headers/x-api-key",
	"otel.trace_exporter":   "codex/otel/trace_exporter/otlp-grpc/headers/authorization",
	"otel.metrics_exporter": "codex/otel/metrics_exporter/otlp-http/headers/x-api-key",
	"mitm hook, first":      "codex/permissions/work/network/mitm/hooks/github/headers/x-token/0",
	"mitm hook, second":     "codex/permissions/work/network/mitm/hooks/github/headers/x-token/1",
}

func TestSnapshot_OtelAndMitmHeadersAreSecrets(t *testing.T) {
	cfgOf := func(plain string) harness.File {
		h := homeWith(t, map[string]string{"config.toml": fmt.Sprintf(secretRoutesConfig, plain)})
		return file(t, snapshot(t, h), "config.toml")
	}
	a, b := cfgOf("plain-one"), cfgOf("plain-two")
	if bytes.Contains(a.Content, []byte("plain-one")) {
		t.Errorf("the snapshot carries a secret:\n%s", a.Content)
	}
	if !bytes.Equal(a.Content, b.Content) {
		t.Errorf("rotating the secrets changed the snapshot:\n%s\nthen\n%s", a.Content, b.Content)
	}
	for route, ref := range secretRoutes {
		if !slices.Contains(a.SecretRefs, ref) || !domain.ValidSecretRef(ref) {
			t.Errorf("%s: no valid ref %q in %q", route, ref, a.SecretRefs)
		}
		if !bytes.Contains(a.Content, []byte(harness.SecretMarker(ref))) {
			t.Errorf("%s: no marker for %q:\n%s", route, ref, a.Content)
		}
	}
	if len(a.SecretRefs) != len(secretRoutes) {
		t.Errorf("refs = %q, want only %d", a.SecretRefs, len(secretRoutes))
	}
	for _, keep := range []string{"https://otel.example.com", "api.github.com", "inject"} {
		if !bytes.Contains(a.Content, []byte(keep)) {
			t.Errorf("config.toml lost %s:\n%s", keep, a.Content)
		}
	}
}

// The env of a credential provider (features.network_proxy.credentials.<c>,
// also in a profile) is a list of variable names, not values: it stays.
func TestSnapshot_CredentialProviderEnvNamesStay(t *testing.T) {
	cfg := "[features.network_proxy.credentials.gh]\nenv = [\"GH_TOKEN\"]\n" +
		"[profiles.p.features.network_proxy.credentials.gh]\nenv = [\"GH_TOKEN\"]\n"
	f := file(t, snapshot(t, homeWith(t, map[string]string{"config.toml": cfg})), "config.toml")
	if len(f.SecretRefs) != 0 || strings.Count(string(f.Content), "GH_TOKEN") != 2 {
		t.Errorf("refs = %q\n%s", f.SecretRefs, f.Content)
	}
}
