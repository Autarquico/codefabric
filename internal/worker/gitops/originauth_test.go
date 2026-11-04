package gitops

import (
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestOriginFetchEnvIsControlled(t *testing.T) {
	t.Setenv("HOME", "/home/hostile")
	t.Setenv("XDG_CONFIG_HOME", "/home/hostile/.config")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent")
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.sslVerify")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("http_proxy", "http://not-for-https")
	a := &OriginAuth{ //nolint:gosec // a sentinel, not a credential
		Askpass: "/state/bin/git-askpass", RunDir: "/state/runs/r1",
		Needles: []redact.Needle{{Ref: "origin", Value: []byte("tok-0123456789"), User: "x-access-token"}},
	}
	env, err := OriginEnv(&Git{}, "/state/repos/p1", a)
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	for k, want := range map[string]string{ //nolint:gosec // paths and sentinels, not credentials
		"HOME": "/state/repos/p1", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_ALLOW_PROTOCOL": "https",
		"GIT_ASKPASS": "/state/bin/git-askpass", "CF_ASKPASS_RUN_DIR": "/state/runs/r1", "GIT_TERMINAL_PROMPT": "0",
		"HTTPS_PROXY": "http://proxy.internal:3128", "NO_PROXY": "localhost",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "SSH_AUTH_SOCK", "GIT_SSL_NO_VERIFY", "http_proxy"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is passed on", k)
		}
	}
	// The configuration is exactly ours, in order, and holds no credential.
	config := map[string]string{}
	for i := range 20 {
		k, ok := got["GIT_CONFIG_KEY_"+string(rune('0'+i))]
		if !ok {
			break
		}
		config[k] = got["GIT_CONFIG_VALUE_"+string(rune('0'+i))]
	}
	for k, want := range map[string]string{
		"credential.helper": "", "credential.useHttpPath": "true", "http.followRedirects": "false", "http.sslVerify": "true",
	} {
		if v, ok := config[k]; !ok || v != want {
			t.Errorf("config %s = %q (set %v), want %q", k, v, ok, want)
		}
	}
	if got["GIT_CONFIG_COUNT"] != "5" || len(config) != 5 {
		t.Errorf("config count = %s, keys %v", got["GIT_CONFIG_COUNT"], config)
	}
	if slices.ContainsFunc(env, func(kv string) bool { return strings.Contains(kv, "tok-0123456789") }) {
		t.Error("the environment holds the credential")
	}
	if _, ok := config["http.sslCAInfo"]; ok {
		t.Error("a CA is trusted without the test hook")
	}
}

func TestOriginFetchWithoutAuthIsTheUsers(t *testing.T) {
	f, err := (&Git{}).originFetch("/b", nil)
	if err != nil || f.values != nil || !slices.Equal(f.env, userEnv()) {
		t.Errorf("originFetch(nil) = %+v, %v", f, err)
	}
}

func TestOriginAuthRefusesWhatIsNotHTTPS(t *testing.T) {
	a := &OriginAuth{Askpass: "/s/git-askpass", RunDir: "/s/run"}
	for repo, ok := range map[string]bool{
		"https://example.com/o/r.git":     true,
		"http://example.com/o/r.git":      false,
		"ssh://git@example.com/o/r.git":   false,
		"git@example.com:o/r.git":         false,
		"/srv/git/r.git":                  false,
		"file:///srv/git/r.git":           false,
		"https://u:p@example.com/o/r.git": false,
		"https://u@example.com/o/r.git":   false,
	} {
		if err := a.check(repo); (err == nil) != ok {
			t.Errorf("check(%q) = %v, want ok %v", repo, err, ok)
		}
	}
	if err := (*OriginAuth)(nil).check("/srv/git/r.git"); err != nil {
		t.Errorf("no auth: %v", err)
	}
	for _, bad := range []*OriginAuth{{Askpass: "git-askpass", RunDir: "/s/run"}, {Askpass: "/s/git-askpass", RunDir: "run"}} {
		if err := bad.check("https://example.com/o/r.git"); err == nil {
			t.Errorf("check(%+v) = nil", bad)
		}
	}
}
