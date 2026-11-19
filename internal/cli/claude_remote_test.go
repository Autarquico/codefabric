package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
)

// fakeController answers the two reads that /workers and the status line make, and
// serves them under a certificate that the stored credentials trust.
func fakeController(t *testing.T, workers []v1.Worker, runs []v1.PsEntry) {
	t.Helper()
	isolatedMachine(t)
	ca := newTestCA(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/workers", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(v1.ListWorkersResponse{Workers: workers})
	})
	mux.HandleFunc("/v1/ps", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(v1.PsResponse{Runs: runs})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t)}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	storeBundle(t, paths.CredentialsDir(), ca, srv.Listener.Addr().String())
}

func remoteRun(task, worker string, mode domain.Mode) v1.PsEntry {
	e := v1.PsEntry{Task: v1.PsTask{Name: task}, Worker: &v1.PsWorker{Name: worker, State: domain.WorkerOnline}}
	e.Mode = mode
	e.State = domain.RunRunning
	return e
}

func twoWorkers() []v1.Worker {
	return []v1.Worker{
		{Name: "omarch02", State: domain.WorkerOnline, Compatible: true},
		{Name: "zeus", State: domain.WorkerOnline, Compatible: true},
		{Name: "attic", State: domain.WorkerOffline, Compatible: true},
	}
}

func TestScenario_ClaudeRemoteRuns_VerLosRemotos(t *testing.T) {
	fakeController(t, twoWorkers(), []v1.PsEntry{remoteRun("fix-login", "omarch02", domain.ModeInteractive), remoteRun("nightly", "zeus", domain.ModeHeadless)})

	// The two commands of the skill, as its `!` lines run them.
	code, workers, errOut := run(t, "worker", "list", "--timeout", "10s")
	if code != 0 {
		t.Fatalf("worker list: %d %s", code, errOut)
	}
	for _, want := range []string{"omarch02", "zeus", "online", "attic", "offline"} {
		if !strings.Contains(workers, want) {
			t.Errorf("the Workers lack %q (offline ones are marked by their state):\n%s", want, workers)
		}
	}
	code, ps, errOut := run(t, "ps", "--timeout", "10s")
	if code != 0 {
		t.Fatalf("ps: %d %s", code, errOut)
	}
	for _, want := range []string{"fix-login", "interactive", "omarch02"} {
		if !strings.Contains(ps, want) {
			t.Errorf("the Runs lack %q:\n%s", want, ps)
		}
	}

	skill, err := renderSkill(claudeWorkers, "v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"!`cf worker list --timeout 10s`", "!`cf ps --timeout 10s`", "MODE `interactive`", "remote", "offline"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("the skill lacks %q", want)
		}
	}
}

func TestScenario_ClaudeRemoteRuns_EntrarEnUnRemoto(t *testing.T) {
	for _, k := range []skillKind{claudeWorkers, codexWorkers} {
		skill, err := renderSkill(k, "v1")
		if err != nil {
			t.Fatal(err)
		}
		text := string(skill)
		if !strings.Contains(text, "`cf attach <task>`") || !strings.Contains(text, "Never run `cf attach` yourself") {
			t.Errorf("the %s skill does not give the command and forbid running it:\n%s", k.label, text)
		}
		frontmatter, _, _ := strings.Cut(text, "\n---\n")
		if strings.Contains(frontmatter, "cf attach") || strings.Contains(frontmatter, "Bash(cf *") {
			t.Errorf("the %s skill may run cf attach:\n%s", k.label, frontmatter)
		}
	}
}

func TestScenario_ClaudeRemoteRuns_SinController(t *testing.T) {
	isolatedMachine(t)
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	storeBundle(t, paths.CredentialsDir(), newTestCA(t), "127.0.0.1:1")
	for _, args := range [][]string{{"worker", "list", "--timeout", "10s"}, {"ps", "--timeout", "10s"}} {
		code, out, errOut := run(t, append([]string{"--json"}, args...)...)
		if code == 0 {
			t.Errorf("cf %s succeeded without a Controller", strings.Join(args, " "))
		}
		if got, _ := errorOf(t, out)["code"].(string); got != string(domain.CodeControllerUnreachable) {
			t.Errorf("cf %s: code %q, stdout %q, stderr %q", strings.Join(args, " "), got, out, errOut)
		}
	}
	if code, out, _ := run(t, "claude", "statusline"); code != 0 || out != "" {
		t.Errorf("the status line failed hard or printed noise: %d %q", code, out)
	}
	if code, out, errOut := run(t, "--json", "ps", "--timeout", "-5s"); code == 0 || errorOf(t, out)["code"] != string(domain.CodeInvalidRequest) {
		t.Errorf("a negative --timeout: %d %q %q", code, out, errOut)
	}
	// With no credentials at all it does not fail either.
	isolatedMachine(t)
	if code, out, _ := run(t, "claude", "statusline"); code != 0 || out != "" {
		t.Errorf("without credentials: %d %q", code, out)
	}
}

// A Controller that takes the connection and never answers, as a Tailscale peer that
// is asleep does: only --timeout ends the call.
func TestScenario_ClaudeRemoteRuns_SinController_QueNoContesta(t *testing.T) {
	isolatedMachine(t)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	storeBundle(t, paths.CredentialsDir(), newTestCA(t), ln.Addr().String())
	for _, args := range [][]string{{"ps", "--timeout", "1s"}, {"worker", "list", "--timeout", "1s"}} {
		name := strings.Join(args, " ")
		start := time.Now()
		code, out, _ := run(t, append([]string{"--json"}, args...)...)
		if code == 0 || errorOf(t, out)["code"] != string(domain.CodeControllerUnreachable) {
			t.Errorf("cf %s: %d %q", name, code, out)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("cf %s took %s with --timeout 1s", name, took)
		}
		if code, _, errOut := run(t, args...); code == 0 || !strings.Contains(errOut, string(domain.CodeControllerUnreachable)) {
			t.Errorf("cf %s in human form: %d %q", name, code, errOut)
		}
	}
}

func TestScenario_ClaudeRemoteRuns_StatusLine(t *testing.T) {
	t.Run("the command shows the remote Runs", func(t *testing.T) {
		fakeController(t, twoWorkers(), []v1.PsEntry{
			remoteRun("fix-login", "omarch02", domain.ModeInteractive),
			remoteRun("nightly", "zeus", domain.ModeHeadless),
			remoteRun("docs", "zeus", domain.ModeInteractive),
			remoteRun("api", "zeus", domain.ModeInteractive),
		})
		code, out, errOut := run(t, "claude", "statusline")
		if code != 0 || errOut != "" {
			t.Fatalf("%d %q", code, errOut)
		}
		if want := "⇄ 3 remote: fix-login@omarch02, docs@zeus +1\n"; out != want {
			t.Errorf("line = %q, want %q", out, want)
		}
	})
	t.Run("it prints nothing when no Run is remote", func(t *testing.T) {
		fakeController(t, twoWorkers(), []v1.PsEntry{remoteRun("nightly", "zeus", domain.ModeHeadless)})
		if code, out, _ := run(t, "claude", "statusline"); code != 0 || out != "" {
			t.Errorf("%d %q", code, out)
		}
	})

	settingsOf := func(t *testing.T, home string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")) //nolint:gosec // a path of the test
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	home := func(t *testing.T) string {
		h := t.TempDir()
		t.Setenv("HOME", h)
		return h
	}

	t.Run("install without --statusline leaves the settings alone", func(t *testing.T) {
		h := home(t)
		if code, _, errOut := runClaude(t, "install"); code != 0 {
			t.Fatal(errOut)
		}
		if _, err := os.Stat(filepath.Join(h, ".claude", "settings.json")); !os.IsNotExist(err) {
			t.Errorf("settings were touched: %v", err)
		}
	})
	t.Run("install --statusline sets it and keeps the other keys", func(t *testing.T) {
		h := home(t)
		path := filepath.Join(h, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"model":"opus","permissions":{"allow":["Bash(ls)"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := runClaude(t, "install", "--statusline")
		if code != 0 || !strings.Contains(out, "statusline: installed") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		got := settingsOf(t, h)
		line, _ := got["statusLine"].(map[string]any)
		if line["type"] != "command" || line["command"] != "cf claude statusline" || line["refreshInterval"] != float64(10) {
			t.Errorf("statusLine = %v", got["statusLine"])
		}
		if got["model"] != "opus" || got["permissions"] == nil {
			t.Errorf("other keys changed: %v", got)
		}
	})
	t.Run("the other keys keep their order and their characters", func(t *testing.T) {
		h := home(t)
		path := filepath.Join(h, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		own := "{\n  \"permissions\": {\"allow\": [\"Bash(a && b)\"]},\n  \"env\": {\"X\": \"<y>\"}\n}\n"
		if err := os.WriteFile(path, []byte(own), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runClaude(t, "install", "--statusline"); code != 0 {
			t.Fatal(errOut)
		}
		after, _ := os.ReadFile(path) //nolint:gosec // a path of the test
		if prefix := strings.TrimRight(strings.TrimSuffix(strings.TrimSpace(own), "}"), " \n"); !strings.HasPrefix(string(after), prefix) {
			t.Errorf("the original bytes are not kept:\n%s", after)
		}
		if !strings.Contains(string(after), "\"statusLine\"") || strings.Contains(string(after), `\u00`) {
			t.Errorf("settings:\n%s", after)
		}
		settingsOf(t, h)
	})
	t.Run("an empty object gets the status line without a stray comma", func(t *testing.T) {
		h := home(t)
		path := filepath.Join(h, ".claude", "settings.json")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte("{}\n"), 0o600)
		if code, _, errOut := runClaude(t, "install", "--statusline"); code != 0 {
			t.Fatal(errOut)
		}
		if settingsOf(t, h)["statusLine"] == nil {
			t.Error("no statusLine")
		}
	})
	t.Run("--scope project writes the settings of the repository only", func(t *testing.T) {
		h := home(t)
		repo := t.TempDir()
		if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		t.Chdir(repo)
		if code, _, errOut := runClaude(t, "install", "--statusline", "--scope", "project"); code != 0 {
			t.Fatal(errOut)
		}
		raw, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.json")) //nolint:gosec // a path of the test
		if err != nil || !strings.Contains(string(raw), "cf claude statusline") {
			t.Errorf("the repository settings: %v %s", err, raw)
		}
		for _, elsewhere := range []string{filepath.Join(h, ".claude", "settings.json"), filepath.Join(repo, ".claude", ".claude")} {
			if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
				t.Errorf("%s was written: %v", elsewhere, err)
			}
		}
	})
	t.Run("an existing status line under --dry-run is kept and nothing is written", func(t *testing.T) {
		h := home(t)
		path := filepath.Join(h, ".claude", "settings.json")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		own := `{"statusLine":{"type":"command","command":"mine"}}`
		_ = os.WriteFile(path, []byte(own), 0o600)
		code, out, _ := runClaude(t, "install", "--statusline", "--dry-run")
		if code != 0 || !strings.Contains(out, "statusline: kept") {
			t.Fatalf("%d %q", code, out)
		}
		if after, _ := os.ReadFile(path); string(after) != own { //nolint:gosec // a path of the test
			t.Errorf("the file changed: %s", after)
		}
	})
	for _, own := range []string{`{"statusLine":{"type":"command","command":"mine"}}`, `{"statusLine":null}`} {
		t.Run("an existing status line is kept and said so: "+own, func(t *testing.T) {
			h := home(t)
			path := filepath.Join(h, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(own), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, _ := runClaude(t, "install", "--statusline")
			if code != 0 || !strings.Contains(out, "statusline: kept") || !strings.Contains(out, "not replaced") {
				t.Fatalf("%d %q", code, out)
			}
			if after, _ := os.ReadFile(path); string(after) != own { //nolint:gosec // a path of the test
				t.Errorf("the file changed: %s", after)
			}
		})
	}
	t.Run("--dry-run says what it would write and writes nothing", func(t *testing.T) {
		h := home(t)
		code, out, _ := runClaude(t, "install", "--statusline", "--dry-run")
		if code != 0 || !strings.Contains(out, "statusline: would be installed") {
			t.Fatalf("%d %q", code, out)
		}
		if _, err := os.Stat(filepath.Join(h, ".claude", "settings.json")); !os.IsNotExist(err) {
			t.Errorf("dry run wrote: %v", err)
		}
	})
	t.Run("settings that are not JSON are an error and stay as they are", func(t *testing.T) {
		h := home(t)
		path := filepath.Join(h, ".claude", "settings.json")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte("{not json"), 0o600)
		if code, _, errOut := runClaude(t, "install", "--statusline"); code == 0 || !strings.Contains(errOut, "CF_INVALID_REQUEST") {
			t.Errorf("%d %q", code, errOut)
		}
		if after, _ := os.ReadFile(path); string(after) != "{not json" { //nolint:gosec // a path of the test
			t.Errorf("the file changed: %s", after)
		}
	})
	t.Run("codex has no --statusline", func(t *testing.T) {
		home(t)
		if code, _, _ := runCodex(t, "install", "--statusline"); code == 0 {
			t.Error("codex accepted --statusline")
		}
	})
}
