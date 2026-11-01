package doctor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/globalapply"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

const secretValue = "sk-ant-oat01-TOP-SECRET-VALUE" //nolint:gosec // a fake token the tests must never see leaked

// host is a machine in a temporary home, with the binaries it is told it has.
type host struct {
	home     string
	binaries map[string]bool
	// commands maps the argv[0] of a command to its exit code; one that is not
	// there cannot run.
	commands map[string]int
	ran      [][]string
}

func (h *host) Home() string           { return h.home }
func (h *host) Environ() []string      { return []string{"HOME=" + h.home} }
func (h *host) Getenv(k string) string { return map[string]string{"HOME": h.home}[k] }
func (h *host) Clock() clock.Clock     { return clock.Real{} }
func (h *host) LookPath(f string) (string, error) {
	if h.binaries[f] {
		return "/usr/bin/" + f, nil
	}
	return "", exec.ErrNotFound
}

func (h *host) Run(_ context.Context, c harness.Cmd) (harness.CmdResult, error) {
	h.ran = append(h.ran, c.Argv)
	code, ok := h.commands[c.Argv[0]]
	if !ok {
		return harness.CmdResult{}, exec.ErrNotFound
	}
	return harness.CmdResult{ExitCode: code, Stdout: []byte("git version 2.50.0\n" + secretValue)}, nil
}

func newHost(t *testing.T) *host {
	t.Helper()
	return &host{home: t.TempDir(), binaries: map[string]bool{"claude": true, "codex": true}, commands: map[string]int{"git": 0}}
}

func plenty(string) (sysinfo.Disk, error) {
	return sysinfo.Disk{FreeBytes: 60, TotalBytes: 100}, nil
}

func newDoctor(t *testing.T, h *host, adapters ...harness.Adapter) (*doctor.Doctor, *secretsfile.Store) {
	t.Helper()
	store := secretsfile.New(filepath.Join(t.TempDir(), "secrets.json"), clock.Real{})
	return doctor.New(doctor.Options{
		Host: h, Adapters: adapters, Worker: "box", DataDir: t.TempDir(), Secrets: store,
		Runtime: func(context.Context) error { return nil }, Disk: plenty, GOOS: "linux",
		Applied: func(context.Context, harness.Adapter) ([]globalapply.Applied, error) { return nil, nil },
	}), store
}

func find(t *testing.T, cs []v1.ComponentHealth, name string) v1.ComponentHealth {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no component %q in %+v", name, cs)
	return v1.ComponentHealth{}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalChecks(t *testing.T) {
	h := newHost(t)
	d, _ := newDoctor(t, h)
	cs := d.Check(t.Context())
	for _, name := range []string{"runtime", "git", "disk"} {
		if c := find(t, cs, name); c.Status != v1.ComponentOK {
			t.Errorf("%s = %+v, want ok", name, c)
		}
	}
	if got := find(t, cs, "git").Message; got != "git version 2.50.0" {
		t.Errorf("git message %q: only the first line of the output is kept", got)
	}
}

func TestRuntimeAndGitFail(t *testing.T) {
	h := newHost(t)
	delete(h.commands, "git")
	store := secretsfile.New(filepath.Join(t.TempDir(), "s.json"), clock.Real{})
	d := doctor.New(doctor.Options{
		Host: h, Worker: "box", DataDir: t.TempDir(), Secrets: store, Disk: plenty,
		Runtime: func(context.Context) error { return errors.New("tmux: not available") },
	})
	cs := d.Check(t.Context())
	rt, git := find(t, cs, "runtime"), find(t, cs, "git")
	if rt.Status != v1.ComponentFail || !strings.Contains(rt.Action, "tmux") || !strings.Contains(rt.Action, "box") {
		t.Errorf("runtime = %+v", rt)
	}
	if git.Status != v1.ComponentFail || !strings.Contains(git.Action, "git") {
		t.Errorf("git = %+v", git)
	}
	if h := doctor.Health(cs); h.Status != v1.HealthDegraded || doctor.Failing(cs) != 2 {
		t.Errorf("health = %+v, failing %d", h, doctor.Failing(cs))
	}
}

func TestHealthIsOKWithoutFailures(t *testing.T) {
	cs := []v1.ComponentHealth{{Name: "a", Status: v1.ComponentOK}, {Name: "b", Status: v1.ComponentWarn}}
	if h := doctor.Health(cs); h.Status != v1.HealthOK || h.Message != "" || len(h.Components) != 2 {
		t.Errorf("health = %+v", h)
	}
}

func TestDiskFailsBelowTheThresholdAndRecoversWithHysteresis(t *testing.T) {
	free := uint64(25)
	h := newHost(t)
	d := doctor.New(doctor.Options{
		Host: h, Worker: "box", DataDir: t.TempDir(),
		Runtime: func(context.Context) error { return nil },
		Disk:    func(string) (sysinfo.Disk, error) { return sysinfo.Disk{FreeBytes: free, TotalBytes: 100}, nil },
	})
	status := func() string { return find(t, d.Check(t.Context()), "disk").Status }
	for _, step := range []struct {
		free uint64
		want string
	}{
		{25, v1.ComponentOK},
		{19, v1.ComponentFail},
		{20, v1.ComponentFail},
		{21, v1.ComponentFail},
		{22, v1.ComponentOK},
		{21, v1.ComponentOK},
		{19, v1.ComponentFail},
	} {
		free = step.free
		if got := status(); got != step.want {
			t.Fatalf("free %d%%: %s, want %s", step.free, got, step.want)
		}
	}
	free = 19
	if c := find(t, d.Check(t.Context()), "disk"); !strings.Contains(c.Action, "libera espacio") {
		t.Errorf("action %q", c.Action)
	}
}

func TestDiskThresholdInBytes(t *testing.T) {
	threshold, err := doctor.ParseThreshold("50GiB")
	if err != nil {
		t.Fatal(err)
	}
	const gib = 1 << 30
	d := doctor.New(doctor.Options{
		Host: newHost(t), DataDir: t.TempDir(), MinFree: threshold, Runtime: func(context.Context) error { return nil },
		Disk: func(string) (sysinfo.Disk, error) {
			return sysinfo.Disk{FreeBytes: 40 * gib, TotalBytes: 1000 * gib}, nil
		},
	})
	if c := find(t, d.Check(t.Context()), "disk"); c.Status != v1.ComponentFail {
		t.Errorf("40GiB free of a 50GiB minimum = %+v", c)
	}
}

func TestDiskThatCannotBeMeasuredWarns(t *testing.T) {
	d := doctor.New(doctor.Options{
		Host: newHost(t), DataDir: t.TempDir(), Runtime: func(context.Context) error { return nil },
		Disk: func(string) (sysinfo.Disk, error) { return sysinfo.Disk{}, errors.New("statfs") },
	})
	if c := find(t, d.Check(t.Context()), "disk"); c.Status != v1.ComponentWarn {
		t.Errorf("disk = %+v", c)
	}
}

func TestHarnessNotInstalledIsNotOffered(t *testing.T) {
	h := newHost(t)
	h.binaries = nil
	d, _ := newDoctor(t, h, claude.Adapter{}, codex.Adapter{})
	for _, c := range d.Check(t.Context()) {
		if strings.Contains(c.Name, ":") {
			t.Errorf("%+v for a harness that is not installed", c)
		}
	}
}

func TestClaudeAuth(t *testing.T) {
	login := func(t *testing.T, h *host) {
		writeFile(t, filepath.Join(h.home, ".claude", ".credentials.json"), `{"token":"`+secretValue+`"}`)
	}
	cases := []struct {
		name   string
		goos   string
		setup  func(t *testing.T, h *host, s *secretsfile.Store)
		want   string
		action string
	}{
		{name: "nothing", want: v1.ComponentFail, action: "claude login en box"},
		{name: "secret in the store", want: v1.ComponentOK, setup: func(t *testing.T, _ *host, s *secretsfile.Store) {
			if err := s.Set(t.Context(), claude.OAuthTokenRef, []byte(secretValue), nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "credentials file", want: v1.ComponentOK, setup: func(t *testing.T, h *host, _ *secretsfile.Store) { login(t, h) }},
		{name: "keychain on macOS", goos: "darwin", want: v1.ComponentOK, setup: func(_ *testing.T, h *host, _ *secretsfile.Store) { h.commands["security"] = 0 }},
		{name: "no keychain item on macOS", goos: "darwin", want: v1.ComponentFail, setup: func(_ *testing.T, h *host, _ *secretsfile.Store) { h.commands["security"] = 44 }, action: "claude login en box"},
		{name: "keychain is not asked on linux", goos: "linux", want: v1.ComponentFail, setup: func(_ *testing.T, h *host, _ *secretsfile.Store) { h.commands["security"] = 0 }, action: "claude login en box"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			dir := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			store := secretsfile.New(filepath.Join(dir, "secrets.json"), clock.Real{})
			if tc.setup != nil {
				tc.setup(t, h, store)
			}
			goos := tc.goos
			if goos == "" {
				goos = "linux"
			}
			d := doctor.New(doctor.Options{
				Host: h, Adapters: []harness.Adapter{claude.Adapter{}}, Worker: "box", DataDir: t.TempDir(), Secrets: store, GOOS: goos,
				Runtime: func(context.Context) error { return nil }, Disk: plenty,
			})
			c := find(t, d.Check(t.Context()), "auth:claude-code")
			if c.Status != tc.want || c.Action != tc.action {
				t.Errorf("auth = %+v, want %s / %q", c, tc.want, tc.action)
			}
			if strings.Contains(c.Message+c.Action, secretValue) {
				t.Errorf("the check revealed a secret: %+v", c)
			}
			if tc.goos == "darwin" {
				i := slices.IndexFunc(h.ran, func(argv []string) bool { return argv[0] == "security" })
				if i < 0 || slices.Contains(h.ran[i], "-w") || slices.Contains(h.ran[i], "-g") {
					t.Errorf("keychain commands = %v: must ask for existence only", h.ran)
				}
			}
		})
	}
}

func TestCodexAuth(t *testing.T) {
	h := newHost(t)
	d, _ := newDoctor(t, h, codex.Adapter{})
	c := find(t, d.Check(t.Context()), "auth:codex")
	if c.Status != v1.ComponentFail || c.Action != "codex login en box" {
		t.Errorf("without auth.json: %+v", c)
	}
	writeFile(t, filepath.Join(h.home, ".codex", "auth.json"), `{"OPENAI_API_KEY":"`+secretValue+`"}`)
	c = find(t, d.Check(t.Context()), "auth:codex")
	if c.Status != v1.ComponentOK || strings.Contains(c.Message+c.Action, secretValue) {
		t.Errorf("with auth.json: %+v", c)
	}
}

func TestHarnessConfigIssuesWarn(t *testing.T) {
	h := newHost(t)
	writeFile(t, filepath.Join(h.home, ".claude", "settings.json"), "{not json")
	d, _ := newDoctor(t, h, claude.Adapter{})
	cs := d.Check(t.Context())
	if c := find(t, cs, "harness:claude-code"); c.Status != v1.ComponentWarn {
		t.Errorf("harness = %+v", c)
	}
	h2 := newHost(t)
	d2, _ := newDoctor(t, h2, claude.Adapter{})
	if c := find(t, d2.Check(t.Context()), "harness:claude-code"); c.Status != v1.ComponentOK {
		t.Errorf("clean config = %+v", c)
	}
}

func TestWorkflowComponent(t *testing.T) {
	cases := []struct {
		name    string
		applied []globalapply.Applied
		err     error
		want    string
	}{
		{name: "nothing applied", want: v1.ComponentOK},
		{name: "clean", applied: []globalapply.Applied{{Name: "AGENTS.md", Ownership: harness.Managed}}, want: v1.ComponentOK},
		{name: "drift", applied: []globalapply.Applied{{Name: "AGENTS.md", Ownership: harness.Managed, Drift: true}}, want: v1.ComponentWarn},
		{name: "partial", applied: []globalapply.Applied{{Name: "skills", Ownership: harness.Managed, Missing: true}}, want: v1.ComponentWarn},
		{name: "local components are the person's", applied: []globalapply.Applied{{Name: "x", Ownership: harness.Local, Drift: true}}, want: v1.ComponentOK},
		{name: "unreadable baseline", err: errors.New("corrupt"), want: v1.ComponentWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := doctor.New(doctor.Options{
				Host: newHost(t), Adapters: []harness.Adapter{codex.Adapter{}}, Worker: "box", DataDir: t.TempDir(),
				Runtime: func(context.Context) error { return nil }, Disk: plenty,
				Applied: func(context.Context, harness.Adapter) ([]globalapply.Applied, error) { return tc.applied, tc.err },
			})
			c := find(t, d.Check(t.Context()), "workflow:codex")
			if c.Status != tc.want {
				t.Errorf("workflow = %+v, want %s", c, tc.want)
			}
			if tc.want == v1.ComponentWarn && tc.err == nil && c.Action != "cf workflow status --worker box" {
				t.Errorf("action = %q", c.Action)
			}
		})
	}
}

func TestCheckTextsFitAHeartbeat(t *testing.T) {
	var drifted []globalapply.Applied
	for i := range 1000 {
		drifted = append(drifted, globalapply.Applied{Name: fmt.Sprintf("skills/with\nnewline-%d", i), Ownership: harness.Managed, Drift: true})
	}
	d := doctor.New(doctor.Options{
		Host: newHost(t), Adapters: []harness.Adapter{codex.Adapter{}}, Worker: "box", DataDir: t.TempDir(),
		Runtime: func(context.Context) error { return errors.New("tmux\x1b[2J broke") }, Disk: plenty,
		Applied: func(context.Context, harness.Adapter) ([]globalapply.Applied, error) { return drifted, nil },
	})
	cs := d.Check(t.Context())
	for _, c := range append(cs, v1.ComponentHealth{Name: "summary", Message: doctor.Health(cs).Message}) {
		for _, s := range []string{c.Name, c.Message, c.Action} {
			if len(s) > doctor.MaxText || !utf8.ValidString(s) || strings.ContainsAny(s, "\n\x1b") {
				t.Errorf("%s: %q", c.Name, s)
			}
		}
	}
	if c := find(t, cs, "workflow:codex"); !strings.HasSuffix(c.Message, "…") {
		t.Errorf("a long text ends in an ellipsis: %q", c.Message)
	}
}

func TestClean(t *testing.T) {
	if got := doctor.Clean("a\nb\u202ec"); got != "a?b?c" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 200)
	if got := doctor.Clean(long); len(got) > doctor.MaxText || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("got %d bytes %q", len(got), got)
	}
}
