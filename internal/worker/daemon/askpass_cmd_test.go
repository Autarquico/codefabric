package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

const askpassToken = "tok-askpass-0123456789"

func grant(origin string) secretjit.Grant {
	return secretjit.Grant{Ref: "github.personal", Origin: origin, User: "x-access-token", Value: askpassToken}
}

func TestAskpassAnswer(t *testing.T) {
	const origin = "https://Example.com/org/repo.git"
	user := func(dest string) string { return "Username for '" + dest + "': " }
	pass := func(dest string) string { return "Password for '" + dest + "': " }
	asUser := func(dest string) string {
		scheme, rest, _ := strings.Cut(dest, "://")
		return pass(scheme + "://x-access-token@" + rest)
	}
	tests := []struct {
		name, origin, prompt, want string
	}{
		{"username", origin, user("https://example.com/org/repo.git"), "x-access-token"},
		{"password", origin, asUser("https://example.com/org/repo.git"), askpassToken},
		{"host in upper case", origin, asUser("https://EXAMPLE.com/org/repo.git"), askpassToken},
		{"default port", origin, asUser("https://example.com:443/org/repo.git"), askpassToken},
		{"no .git in the prompt", origin, asUser("https://example.com/org/repo"), askpassToken},
		{"trailing slash", origin, asUser("https://example.com/org/repo.git/"), askpassToken},
		{"no .git in the grant", "https://example.com/org/repo", asUser("https://example.com/org/repo.git"), askpassToken},
		{"explicit port both", "https://git.example.com:8443/o/r.git", asUser("https://git.example.com:8443/o/r.git"), askpassToken},
		{"a path with spaces", "https://example.com/my%20org/my%20repo.git", asUser("https://example.com/my org/my repo.git"), askpassToken},

		{"other host", origin, asUser("https://evil.example/org/repo.git"), ""},
		{"a subdomain", origin, asUser("https://a.example.com/org/repo.git"), ""},
		{"a host that ends like it", origin, asUser("https://notexample.com/org/repo.git"), ""},
		{"other repo", origin, asUser("https://example.com/org/repo2.git"), ""},
		{"a prefix of the path", origin, asUser("https://example.com/org/repo.git/sub"), ""},
		{"another case of the path", origin, asUser("https://example.com/org/Repo.git"), ""},
		{"no path", origin, user("https://example.com"), ""},
		{"other port", origin, asUser("https://example.com:8443/org/repo.git"), ""},
		{"grant port, prompt default", "https://example.com:8443/org/repo.git", asUser("https://example.com/org/repo.git"), ""},
		{"http", origin, asUser("http://example.com/org/repo.git"), ""},
		{"ssh", origin, user("ssh://example.com/org/repo.git"), ""},
		{"a grant that is http", "http://example.com/org/repo.git", asUser("https://example.com/org/repo.git"), ""},
		{"a grant with userinfo", "https://u:p@example.com/org/repo.git", asUser("https://example.com/org/repo.git"), ""},
		{"a grant with a query", "https://example.com/org/repo.git?x=1", asUser("https://example.com/org/repo.git"), ""},
		{"another user", origin, pass("https://someone@example.com/org/repo.git"), ""},
		{"a password with no user", origin, pass("https://example.com/org/repo.git"), ""},
		{"a username prompt with userinfo", origin, user("https://x-access-token@example.com/org/repo.git"), ""},
		{"userinfo hiding a host", origin, pass("https://x-access-token@evil.example@example.com/org/repo.git"), ""},
		{"a host in the userinfo", origin, pass("https://example.com@evil.example/org/repo.git"), ""},
		{"no closing", origin, "Username for 'https://example.com/org/repo.git'", ""},
		{"text after", origin, user("https://example.com/org/repo.git") + "x", ""},
		{"another prompt", origin, "Passphrase for 'https://example.com/org/repo.git': ", ""},
		{"empty", origin, "", ""},
		{"a control character", origin, user("https://example.com/org/repo.git\n"), ""},
		{"a NUL", origin, user("https://example.com/org/repo.git\x00"), ""},
		{"a query in the prompt", origin, user("https://example.com/org/repo.git?a=b"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := askpassAnswer(tc.prompt, grant(tc.origin))
			if ok != (tc.want != "") || got != tc.want {
				t.Errorf("askpassAnswer(%q) = %q, %v; want %q", tc.prompt, got, ok, tc.want)
			}
		})
	}
}

func TestAskpassAnswerRefusesValuesThatBreakTheFraming(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "a\x00b", ""} {
		g := grant("https://example.com/o/r.git")
		g.Value = value
		if got, ok := askpassAnswer("Password for 'https://x-access-token@example.com/o/r.git': ", g); ok {
			t.Errorf("answered %q for a value with %q", got, value)
		}
	}
	g := grant("https://example.com/o/r.git")
	g.User = "a\nb"
	if _, ok := askpassAnswer("Username for 'https://example.com/o/r.git': ", g); ok {
		t.Error("answered a user with a newline")
	}
}

// delivered is a run directory with the origin credential delivered to askpass.
func delivered(t *testing.T, consumers ...secretjit.Consumer) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := secretjit.New()
	err := m.DeliverFor(dir, 1, consumers, []redact.Needle{{Ref: "github.personal", Value: []byte(askpassToken)}}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(dir, "github.personal", secretjit.Binding{Origin: "https://example.com/org/repo.git", User: "x-access-token"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ask(runDir, prompt string) (string, error) {
	var out bytes.Buffer
	err := answerAskpass(&out, runDir, prompt)
	return out.String(), err
}

const passwordPrompt = "Password for 'https://x-access-token@example.com/org/repo.git': "

func TestAnswerAskpassReadsTheDeliveryOfTheRun(t *testing.T) {
	dir := delivered(t, secretjit.Askpass)
	if out, err := ask(dir, passwordPrompt); err != nil || out != askpassToken+"\n" {
		t.Errorf("answer = %q, %v", out, err)
	}
	if out, err := ask(dir, "Username for 'https://example.com/org/repo.git': "); err != nil || out != "x-access-token\n" {
		t.Errorf("username = %q, %v", out, err)
	}
	if out, err := ask(dir, "Password for 'https://x-access-token@example.com/other/repo.git': "); err == nil || out != "" {
		t.Errorf("another repo: %q, %v", out, err)
	}
	if err := secretjit.New().Revoke(dir, secretjit.Askpass, nil); err != nil {
		t.Fatal(err)
	}
	if out, err := ask(dir, passwordPrompt); err == nil || out != "" {
		t.Errorf("after the revocation: %q, %v", out, err)
	}
}

func TestAnswerAskpassNeedsAccessAndARunDirectory(t *testing.T) {
	if out, err := ask(delivered(t, secretjit.Harness), passwordPrompt); err == nil || out != "" {
		t.Errorf("a credential of the harness only: %q, %v", out, err)
	}
	for _, dir := range []string{"", "relative/run", filepath.Join(t.TempDir(), "missing")} {
		if out, err := ask(dir, passwordPrompt); err == nil || out != "" {
			t.Errorf("run dir %q: %q, %v", dir, out, err)
		}
	}
}

func TestGitAskpassCommandAnswersOnStdoutOnly(t *testing.T) {
	t.Setenv(askpassRunDirEnv, delivered(t, secretjit.Askpass))
	var stdout, stderr bytes.Buffer
	cmd := newGitAskpassCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{passwordPrompt})
	if err := cmd.Execute(); err != nil || stdout.String() != askpassToken+"\n" || stderr.Len() != 0 {
		t.Fatalf("answer = %q, stderr %q, %v", stdout.String(), stderr.String(), err)
	}
	stdout.Reset()
	cmd = newGitAskpassCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"Password for 'https://x-access-token@evil.example/org/repo.git': "})
	err := cmd.Execute()
	if err == nil || stdout.Len() != 0 || strings.Contains(err.Error()+stderr.String(), "evil") || strings.Contains(err.Error()+stderr.String(), askpassToken) {
		t.Fatalf("refusal = %q, stderr %q, %v", stdout.String(), stderr.String(), err)
	}
}

func TestAskpassScriptQuotesEveryWord(t *testing.T) {
	got, err := AskpassScript([]string{"/opt/my dir/it's here/cf", "worker", "git-askpass"})
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\nexec '/opt/my dir/it'\\''s here/cf' 'worker' 'git-askpass' \"$@\"\n"
	if got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
	for _, bad := range [][]string{nil, {"cf"}, {"/cf\x00"}} {
		if _, err := AskpassScript(bad); err == nil {
			t.Errorf("script for %q", bad)
		}
	}
}

func TestEnsureAskpassScriptRunsWithSpacesAndQuotes(t *testing.T) {
	root := t.TempDir()
	helperDir := filepath.Join(root, "my dir", "it's here")
	if err := os.MkdirAll(helperDir, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(helperDir, "cf")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '[%s]' \"$a\"; done\n"), 0o700); err != nil { //nolint:gosec // a test helper
		t.Fatal(err)
	}
	paths := config.Paths{ConfigDir: filepath.Join(root, "cfg dir"), DataDir: filepath.Join(root, "data dir")}
	path, err := EnsureAskpassScript(paths, []string{helper, "worker", "git-askpass"})
	if err != nil {
		t.Fatal(err)
	}
	if path != AskpassScriptPath(paths) || !filepath.IsAbs(path) {
		t.Fatalf("path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("script: %v, %v", info, err)
	}
	out, err := exec.CommandContext(t.Context(), path, "Username for 'https://h/a b': ").Output() //nolint:gosec // the script under test
	if err != nil || string(out) != "[worker][git-askpass][Username for 'https://h/a b': ]" {
		t.Fatalf("running the script: %q, %v", out, err)
	}

	// The same content is left alone; another is replaced whole, with no leftovers.
	before, _ := os.Stat(path)
	if _, err := EnsureAskpassScript(paths, []string{helper, "worker", "git-askpass"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Error("an identical script was rewritten")
	}
	if _, err := EnsureAskpassScript(paths, []string{helper, "worker", "git-askpass", "--x"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "'--x'") { //nolint:gosec // the script under test
		t.Errorf("script not replaced: %q", data)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("leftovers: %v", entries)
	}
	// A mode that drifted is put back.
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // a test of the repair
		t.Fatal(err)
	}
	if _, err := EnsureAskpassScript(paths, []string{helper, "worker", "git-askpass", "--x"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}
