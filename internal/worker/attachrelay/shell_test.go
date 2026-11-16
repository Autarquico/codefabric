package attachrelay

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// shellRig is a rig whose Worker may open a shell, and that keeps the command it launched.
type shellRig struct {
	*rig
	worktree string
	allow    atomic.Bool
	launched chan cfruntime.Command
	asked    chan [2]any // run and epoch the worktree was asked for
}

func newShellRig(t *testing.T) *shellRig {
	t.Helper()
	s := &shellRig{worktree: t.TempDir(), launched: make(chan cfruntime.Command, 4), asked: make(chan [2]any, 4)}
	s.allow.Store(true)
	s.rig = newRig(t, func(o *Options) {
		o.Start = func(cmd cfruntime.Command, cols, rows uint16) (Terminal, error) {
			s.launched <- cmd
			s.opens <- [2]uint16{cols, rows}
			return s.term, nil
		}
		o.Shell = ShellOptions{
			Allow: s.allow.Load,
			Worktree: func(run string, epoch uint64) string {
				s.asked <- [2]any{run, epoch}
				return s.worktree
			},
			Account: func() (Account, error) { return Account{Name: "ana", Home: "/home/ana", Shell: "/bin/sh"}, nil },
			Lang:    func() string { return "es_ES.UTF-8" },
		}
	})
	return s
}

func (s *shellRig) noLaunch() {
	s.t.Helper()
	select {
	case cmd := <-s.launched:
		s.t.Fatalf("a shell was launched: %+v", cmd)
	default:
	}
}

func TestRelayShell_LaunchesTheLoginShellInTheWorktreeWithItsOwnEnvironment(t *testing.T) {
	t.Setenv("CF_WORKER_TOKEN", "hunter2")
	s := newShellRig(t)
	s.run()
	c := s.ask(v1.AttachModeShell)
	cmd := <-s.launched
	if cmd.Path != "/bin/sh" || !slices.Equal(cmd.Args, []string{"-l"}) || cmd.Dir != s.worktree || !cmd.KillGroup {
		t.Errorf("command = %+v", cmd)
	}
	want := []string{"HOME=/home/ana", "USER=ana", "LOGNAME=ana", "SHELL=/bin/sh", "PATH=" + shellPath, "LANG=es_ES.UTF-8"}
	if !slices.Equal(cmd.Env, want) {
		t.Errorf("env = %v, want %v", cmd.Env, want)
	}
	for _, kv := range cmd.Env {
		if strings.Contains(kv, "hunter2") || strings.HasPrefix(kv, "CF_") {
			t.Errorf("the environment of the Worker leaked: %s", kv)
		}
	}
	if got := <-s.asked; got != [2]any{"run_A", uint64(1)} {
		t.Errorf("the worktree was asked for %v", got)
	}
	// A shell types: what the client sends reaches it.
	if err := v1.WriteFrame(c, v1.DataFrame([]byte("ls\n"))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the shell to be typed into", func() bool { return s.term.typed() == "ls\n" })
}

func TestRelayShell_RefusesWhenTheWorkerDoesNotAllowIt(t *testing.T) {
	s := newShellRig(t)
	s.allow.Store(false)
	s.run()
	f := frame(t, s.ask(v1.AttachModeShell))
	wantError(t, f, domain.CodePolicyDenied)
	if e, _ := v1.ParseError(f.Payload); !strings.Contains(e.Message, domain.PolicyWorkerAllowShell) {
		t.Errorf("the refusal does not name the policy: %q", e.Message)
	}
	s.noLaunch()
}

func TestRelayShell_ARelayWithoutShellOptionsOpensNone(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	wantError(t, frame(t, r.ask(v1.AttachModeShell)), domain.CodePolicyDenied)
}

// The Worker withdraws the shell after the request was checked and before the
// terminal starts: it checks again, and nothing opens.
func TestRelayShell_ChecksAgainRightBeforeLaunching(t *testing.T) {
	s := newShellRig(t)
	var calls atomic.Int32
	s.relay.o.Shell.Allow = func() bool { return calls.Add(1) == 1 } // yes at the request, no at the launch
	s.run()
	wantError(t, frame(t, s.ask(v1.AttachModeShell)), domain.CodePolicyDenied)
	s.noLaunch()
	if calls.Load() != 2 {
		t.Errorf("the setting was read %d times, want 2", calls.Load())
	}
}

func TestRelayShell_NeedsTheRunToBeLiveHereAndItsWorktreeToExist(t *testing.T) {
	s := newShellRig(t)
	s.live = false
	s.run()
	wantError(t, frame(t, s.ask(v1.AttachModeShell)), domain.CodeRunNotAttachable)
	s.noLaunch()

	m := newShellRig(t)
	m.worktree = filepath.Join(m.worktree, "gone")
	m.run()
	wantError(t, frame(t, m.ask(v1.AttachModeShell)), domain.CodeRunNotAttachable)
	m.noLaunch()
}

func TestShellEnv_IsMinimalAndIgnoresTheEnvironmentOfTheProcess(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	env := ShellEnv(Account{Name: "ana", Home: "/h"}, "/bin/zsh", "")
	var keys []string
	for _, kv := range env {
		keys = append(keys, strings.SplitN(kv, "=", 2)[0])
	}
	if !slices.Equal(keys, []string{"HOME", "USER", "LOGNAME", "SHELL", "PATH", "LANG"}) {
		t.Errorf("keys = %v", keys)
	}
	if !slices.Contains(env, "LANG="+shellLang) {
		t.Errorf("no default LANG: %v", env)
	}
}

func TestPasswdShell_FindsTheShellOfAUid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	body := "root:x:0:0:root:/root:/bin/bash\nana:x:1000:1000:Ana,,,:/home/ana:/usr/bin/zsh\nnoshell:x:1001:1001:::\nodd:x:1002\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for uid, want := range map[int]string{0: "/bin/bash", 1000: "/usr/bin/zsh", 1001: "", 1002: "", 4242: ""} {
		if got := PasswdShell(path, uid); got != want {
			t.Errorf("uid %d: %q, want %q", uid, got, want)
		}
	}
	if got := PasswdShell(filepath.Join(t.TempDir(), "none"), 0); got != "" {
		t.Errorf("a missing file gave %q", got)
	}
}

func TestUsableShell_FallsBackToBinSh(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	exe := filepath.Join(dir, "exe")
	for p, mode := range map[string]os.FileMode{plain: 0o600, exe: 0o700} {
		if err := os.WriteFile(p, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	for shell, want := range map[string]string{exe: exe, plain: FallbackShell, "": FallbackShell, "sh": FallbackShell, filepath.Join(dir, "missing"): FallbackShell, dir: FallbackShell} {
		if got := usableShell(shell); got != want {
			t.Errorf("usableShell(%q) = %q, want %q", shell, got, want)
		}
	}
}
