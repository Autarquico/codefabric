package workflowadopt

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Values are assembled at run time so the source holds nothing that looks
// like a secret to a scanner.
var (
	ghToken     = "ghp" + "_" + "aB3dE5gH7jK9mN1pQ3sT5vX7zA9cD1fG3hJ5"
	ghPAT       = "github" + "_pat_" + "11AABBCCD0aB3dE5gH7jK9_mN1pQ3sT5vX7zA9cD1fG3hJ5kL7"
	openaiKey   = "sk" + "-" + "proj-aB3dE5gH7jK9mN1pQ3sT5vX7zA9cD1fG3hJ5kL7"
	antKey      = "sk" + "-ant-" + "api03-aB3dE5gH7jK9mN1pQ3sT5vX7zA9cD1"
	awsKey      = "AKIA" + "Z3XQ7K2M9PL4WN8R"
	slackTok    = "xoxb" + "-1234567890-aB3dE5gH7jK9mN1pQ"
	googleKey   = "AIza" + "Sy" + "A3dE5gH7jK9mN1pQ3sT5vX7zA9cD1fG3h"
	pemBlock    = "-----BEGIN " + "PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nabc123def456\n-----END " + "PRIVATE KEY-----"
	assignedVal = "Tr0ub4dor&3xK9mQz"
	urlPass     = "p4ss" + "W0rd-x9Kq"
	queryTok    = "t0k3n" + "Zq8Lm2Vx7"
	opaqueTok   = "aB3dE5gH7jK9" + "mN1pQ3sT5vX7"
	basicTok    = "dXNlcjpwNHNzVzByZC14OUtx" + "Zg=="
	hexSecret   = "9f86d081884c7d659a2f" + "eaa0c55ad015a3bf4f1b"
)

func snapOf(content string) harness.NativeSnapshot {
	f := harness.File{Path: "config.toml", Mode: 0o644, Content: []byte(content), SecretRefs: harness.SecretRefs([]byte(content))}
	d, _ := harness.FilesDigest([]harness.File{f})
	return harness.NativeSnapshot{Harness: "codex", Components: []harness.Component{{
		Name: "settings", Kind: harness.KindFile, Ownership: harness.Managed, Files: []harness.File{f}, Digest: d,
	}}}
}

func TestScanTruePositives(t *testing.T) {
	cases := []struct{ name, content, kind string }{
		{"github", `GITHUB_TOKEN = "` + ghToken + `"`, "github-token"},
		{"github-bare", "use " + ghToken + " here", "github-token"},
		{"github-pat", `x = "` + ghPAT + `"`, "github-token"},
		{"openai", `OPENAI = "` + openaiKey + `"`, "openai-key"},
		{"anthropic", `A = "` + antKey + `"`, "anthropic-key"},
		{"aws", `id = "` + awsKey + `"`, "aws-access-key"},
		{"slack", `{"hook": "` + slackTok + `"}`, "slack-token"},
		{"google", `k = "` + googleKey + `"`, "google-api-key"},
		{"pem", "key = \"" + pemBlock + "\"", "private-key"},
		{"generic-toml", `DB_PASSWORD = "` + assignedVal + `"`, "assigned-secret"},
		{"generic-json", `{"apiToken": "` + assignedVal + `"}`, "assigned-secret"},
		{"generic-env", "MY_SECRET=" + assignedVal, "assigned-secret"},
		{"url-credentials", `DATABASE = "postgresql://app:` + urlPass + `@db.internal:5432/app"`, "url-credentials"},
		{"url-token-query", `endpoint = "https://api.internal/v1/stream?access_token=` + queryTok + `&v=2"`, "url-token"},
		{"url-key-query", `u = "https://maps.internal/api?x=1&key=` + queryTok + `"`, "url-token"},
		{"url-empty-user", `REDIS = "redis://:` + urlPass + `@cache.internal:6379"`, "url-credentials"},
		{"url-short-password", `u = "postgresql://app:p4ss@db.internal/app"`, "url-credentials"},
		{"bearer", `Authorization = "Bearer ` + opaqueTok + `"`, "authorization-header"},
		{"bearer-header-line", "Authorization: Bearer " + opaqueTok, "authorization-header"},
		{"basic", `{"Authorization": "Basic ` + basicTok + `"}`, "authorization-header"},
		{"hex-secret-under-secret-key", `SENTRY_AUTH_TOKEN = "` + hexSecret + `"`, "assigned-secret"},
		{"hex64-secret-under-secret-key", `hmac_secret = "` + hexSecret + hexSecret[:24] + `"`, "assigned-secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Scan(snapOf(c.content))
			if len(r.Hits) != 1 || r.Hits[0].Kind != c.kind {
				t.Fatalf("hits = %+v, want one %s", r.Hits, c.kind)
			}
			h := r.Hits[0]
			if !h.Replaceable || !domain.ValidSecretRef(h.Ref) || h.Line != 1 || h.Component != "settings" || h.File != "config.toml" {
				t.Fatalf("hit = %+v", h)
			}
		})
	}
}

func TestScanFalsePositives(t *testing.T) {
	for name, content := range map[string]string{ //nolint:gosec // inputs that must not look like secrets
		"uuid":                    `API_KEY = "123e4567-e89b-12d3-a456-426614174000"`,
		"git sha":                 `COMMIT_KEY = "` + strings.Repeat("a1b2c3d4e5", 4) + `"`,
		"sha256":                  `KEY = "` + strings.Repeat("0123456789abcdef", 4) + `"`,
		"path":                    `SECRET_FILE_DIR = "/home/alex/.config/cf/secrets-long-path-1"`,
		"home path":               `key_dir = "~/.ssh/id_ed25519_2024_backup"`,
		"short base64":            `msg = "SGVsbG8gd29ybGQ="`,
		"base64 no key":           `blob = "aGVsbG8gd29ybGQgZnJvbSBjZiBhZG9wdA=="`,
		"sk in prose":             "the task-force risk-assessment needs sk-learn style tasks, see disk-usage-report-quarterly-2024-summary",
		"sk kebab":                "sk-assessment-for-the-quarterly-planning-report",
		"env var name":            `bearer_token_env_var = "MY_SERVICE_TOKEN_2"`,
		"env ref":                 `API_KEY = "${OPENAI_API_KEY_FROM_HOST_1}"`,
		"url":                     `TOKEN_URL = "https://auth.example.com/oauth/token?v=12345678"`,
		"placeholder":             `API_KEY = "your-api-key-goes-here-1234"`,
		"short":                   `PASSWORD = "hunter2"`,
		"identifier":              `secret_name = "database_password_for_prod"`,
		"word key":                `keyboard = "layout-us-international-variant"`,
		"url without credentials": `u = "postgresql://db.internal:5432/app"`,
		"url with user only":      `u = "ssh://git@github.com/org/repo.git"`,
		"url with port":           `u = "http://localhost:8080/path@2"`,
		"url credential ref":      `u = "postgresql://app:${DB_PASSWORD}@db.internal/app"`,
		"url placeholder":         `u = "postgresql://user:your-password-here@host/db"`,
		"query not a secret":      `u = "https://example.org/search?q=tokens&page=2&keyboard=us-international"`,
		"query ref":               `u = "https://api.internal/v1?token=${API_TOKEN_FROM_ENV}"`,
		"query short":             `u = "https://api.internal/v1?key=abc"`,
		"bearer in prose":         "Send the Bearer token in the Authorization header, as the documentation describes.",
		"bearer long word":        "a Bearer authentication-scheme-description is described there",
		"bearer ref":              `Authorization = "Bearer {{cf-secret:codex/mcp/docs/env/T}}"`,
		"basic in prose":          "Basic configuration-management-practices-overview apply",
		"hex under plain key":     `commit = "` + strings.Repeat("a1b2c3d4e5", 4) + `"`,
		"flag with a port":        `args = ["--port", "8080"]`,
		"flag with a model":       `args = ["-y", "srv", "--model", "gpt-5"]`,
		"flag naming a file":      `args = ["--token-file", "/run/secrets/token"]`,
		"flag naming an env var":  `args = ["--api-key-env", "MY_SERVICE_API_KEY_2"]`,
		"flag with a reference":   `args = ["--api-key", "${CONTEXT7_API_KEY_FROM_HOST}"]`,
		"flag with a short value": `args = ["--token", "abc"]`,
		"flag without a value":    `args = ["--api-key", "--verbose"]`,
		"url empty password":      `u = "redis://:@cache.internal:6379"`,
		"url reference password":  `u = "redis://:${REDIS_PASSWORD}@cache.internal"`,
		"existing marker":         `API_KEY = "{{cf-secret:codex/mcp/docs/env/API_KEY}}"`,
	} {
		t.Run(name, func(t *testing.T) {
			if hits := Scan(snapOf(content)).Hits; len(hits) != 0 {
				t.Fatalf("false positive: %+v", hits)
			}
		})
	}
}

func TestScanListsSubstitutedRefs(t *testing.T) {
	r := Scan(snapOf("a = \"{{cf-secret:codex/mcp/docs/env/A}}\"\nb = \"{{cf-secret:codex/mcp/docs/env/B}}\""))
	if len(r.Substituted) != 2 || r.Substituted[1].Ref != "codex/mcp/docs/env/B" || r.Substituted[0].File != "config.toml" {
		t.Fatalf("substituted = %+v", r.Substituted)
	}
}

func TestScanLineAndRefDeterminism(t *testing.T) {
	content := "a = 1\n[mcp_servers.docs.env]\nGITHUB_TOKEN = \"" + ghToken + "\"\nOTHER_TOKEN = \"" + ghToken + "\"\n"
	r1, r2 := Scan(snapOf(content)), Scan(snapOf(content))
	if len(r1.Hits) != 2 || r1.Hits[0].Line != 3 || r1.Hits[1].Line != 4 {
		t.Fatalf("hits = %+v", r1.Hits)
	}
	if r1.Hits[0].Ref != r2.Hits[0].Ref || r1.Hits[0].Ref == r1.Hits[1].Ref {
		t.Fatalf("refs not deterministic/unique: %q %q", r1.Hits[0].Ref, r1.Hits[1].Ref)
	}
	if r1.Hits[0].Ref != "adopt/codex/settings/config.toml/GITHUB_TOKEN" {
		t.Fatalf("ref = %q", r1.Hits[0].Ref)
	}
}

func TestScanArgsNotReplaceable(t *testing.T) {
	for name, content := range map[string]string{
		"toml":                               "[mcp_servers.x]\ncommand = \"npx\"\nargs = [\"-y\", \"srv\", \"--token\", \"" + ghToken + "\"]\n",
		"multiline":                          "args = [\n  \"-y\",\n  \"" + ghToken + "\",\n]\n",
		"url":                                `args = ["-y", "pg-mcp", "postgresql://app:` + urlPass + `@db/app"]`,
		"flag and value apart (toml)":        `args = ["-y", "ctx7", "--api-key", "` + assignedVal + `"]`,
		"flag and value apart (json)":        `{"mcpServers":{"x":{"args":["--access-token", "` + assignedVal + `"]}}}`,
		"flag and value on lines":            "args = [\n  \"--token\",\n  \"" + assignedVal + "\",\n]\n",
		"flag before the flag (SEC-11)":      `args = ["--token", "--api-key", "` + assignedVal + `"]`,
		"value that starts with a dash":      `args = ["--api-key", "-` + assignedVal + `"]`,
		"flag with key in its name (SEC-11)": `args = ["--no-key-cache", "--api-key", "` + assignedVal + `"]`,
		"json":                               `{"mcpServers":{"x":{"args":["--key=` + ghToken + `"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := Scan(snapOf(content))
			if len(r.Hits) != 1 || r.Hits[0].Replaceable || r.Hits[0].Reason == "" {
				t.Fatalf("hits = %+v", r.Hits)
			}
			_, err := Apply(snapOf(content))
			if !errors.Is(err, ErrNotReplaceable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// After the args array is closed, a value is replaceable again.
	r := Scan(snapOf("args = [\"a\"]\nGITHUB_TOKEN = \"" + ghToken + "\"\n"))
	if len(r.Hits) != 1 || !r.Hits[0].Replaceable {
		t.Fatalf("hits = %+v", r.Hits)
	}
}

func TestApplyReplacesAndRecomputes(t *testing.T) {
	content := "[mcp_servers.docs.env]\nGITHUB_TOKEN = \"" + ghToken + "\"\nDB_PASSWORD = \"" + assignedVal + "\"\nK = \"{{cf-secret:codex/mcp/docs/env/K}}\"\n"
	in := snapOf(content)
	r := Scan(in)
	out, err := Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	f := out.Components[0].Files[0]
	for _, h := range r.Hits {
		if !strings.Contains(string(f.Content), harness.SecretMarker(h.Ref)) {
			t.Fatalf("marker of %s missing in %s", h.Ref, f.Content)
		}
	}
	if err := f.CheckSecrets(); err != nil {
		t.Fatal(err)
	}
	if len(f.SecretRefs) != 3 {
		t.Fatalf("refs = %v", f.SecretRefs)
	}
	want, _ := harness.FilesDigest(out.Components[0].Files)
	if out.Components[0].Digest != want || out.Components[0].Digest == in.Components[0].Digest {
		t.Fatalf("digest not recomputed")
	}
	if string(in.Components[0].Files[0].Content) != content {
		t.Fatal("input modified")
	}
	if left := Scan(out).Hits; len(left) != 0 {
		t.Fatalf("hits after apply: %+v", left)
	}
}

func TestApplyKeepsUntouchedComponents(t *testing.T) {
	in := snapOf(`GITHUB_TOKEN = "` + ghToken + `"`)
	clean := harness.File{Path: "AGENTS.md", Mode: 0o644, Content: []byte("hello")}
	in.Components = append(in.Components, harness.Component{Name: "agents", Files: []harness.File{clean}, Digest: "sha256:keep"})
	out, err := Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Components[1].Digest != "sha256:keep" {
		t.Fatalf("digest = %q", out.Components[1].Digest)
	}
}

// Property: after Apply no value is anywhere in the snapshot, and no value is
// in an error or a preview.
func TestNoValueSurvives(t *testing.T) {
	values := []string{ghToken, ghPAT, openaiKey, antKey, awsKey, slackTok, googleKey, assignedVal, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"}
	content := strings.Join([]string{
		`GITHUB_TOKEN = "` + ghToken + `"`, `P = "` + ghPAT + `"`, `O = "` + openaiKey + `"`, `A = "` + antKey + `"`,
		`id = "` + awsKey + `"`, `s = "` + slackTok + `"`, `g = "` + googleKey + `"`, `DB_PASSWORD = "` + assignedVal + `"`,
		`pem = "` + pemBlock + `"`,
	}, "\n")
	in := snapOf(content)
	r := Scan(in)
	for _, h := range r.Hits {
		for _, v := range values {
			if strings.Contains(h.Preview+h.Ref+h.Reason+h.where(), v) {
				t.Fatalf("value in finding %+v", h)
			}
		}
	}
	if len(r.Hits) != 9 {
		t.Fatalf("hits = %d, want 9", len(r.Hits))
	}
	out, err := Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	dump := fmt.Sprintf("%+v", out)
	for _, c := range out.Components {
		for _, f := range c.Files {
			dump += string(f.Content) + strings.Join(f.SecretRefs, ",")
		}
	}
	for _, v := range values {
		if strings.Contains(dump, v) {
			t.Fatalf("value %q... survives in the snapshot", v[:4])
		}
	}
	// Errors name no value either.
	_, err = Apply(snapOf(`args = ["` + ghToken + `"]`))
	if err == nil || strings.Contains(err.Error(), ghToken) {
		t.Fatalf("err = %v", err)
	}
}
