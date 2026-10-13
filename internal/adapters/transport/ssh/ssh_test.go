package ssh_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/transport/ssh"
	"github.com/Autarquico/codefabric/internal/domain"
)

// fakeSSH is an ssh that logs its arguments, one per line, and exits with the
// status in the environment.
func fakeSSH(t *testing.T, status string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + log + "'\nexit " + status + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return bin, log
}

func target(t *testing.T, s string) domain.SSHTarget {
	t.Helper()
	tg, err := domain.ParseSSHTarget(s)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatalf("ssh was not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

var tmuxArgv = []string{"/opt/homebrew/bin/tmux", "-L", "cf-0123456789ab", "attach-session", "-t", "=cf-run_x-e1"}

func TestAttach_BuildsTheArgvWithTheDashDashBeforeTheDestination(t *testing.T) {
	bin, log := fakeSSH(t, "0")
	var out, errOut bytes.Buffer
	code, err := ssh.Attach(t.Context(), ssh.Options{Binary: bin}, target(t, "alex@mini:2222"), tmuxArgv, ssh.Stdio{In: strings.NewReader(""), Out: &out, Err: &errOut})
	if err != nil || code != 0 {
		t.Fatalf("Attach = %d, %v", code, err)
	}
	want := []string{
		"-t", "-o", "ConnectTimeout=15", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-p", "2222", "--", "alex@mini",
		"'/opt/homebrew/bin/tmux' '-L' 'cf-0123456789ab' 'attach-session' '-t' '=cf-run_x-e1'",
	}
	if got := lines(t, log); !slices.Equal(got, want) {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}

	code, err = ssh.Attach(t.Context(), ssh.Options{Binary: bin}, target(t, "u@[fe80::1]"), tmuxArgv[:1], ssh.Stdio{})
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if got := lines(t, log); got[slices.Index(got, "--")+1] != "u@fe80::1" || slices.Contains(got, "-p") {
		t.Errorf("IPv6 argv = %q", got)
	}
}

func TestAttach_ReturnsTheExitStatusOfSSH(t *testing.T) {
	for _, status := range []string{"0", "7", "255"} {
		bin, _ := fakeSSH(t, status)
		code, err := ssh.Attach(t.Context(), ssh.Options{Binary: bin}, target(t, "box"), tmuxArgv, ssh.Stdio{})
		if err != nil || strconv.Itoa(code) != status {
			t.Errorf("status %s: code %d, err %v", status, code, err)
		}
	}
	if _, err := ssh.Attach(t.Context(), ssh.Options{Binary: "/nonexistent/ssh"}, target(t, "box"), tmuxArgv, ssh.Stdio{}); err == nil || errors.Is(err, ssh.ErrRejected) {
		t.Errorf("a missing ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	bin, log := fakeSSH(t, "0")
	if _, err := ssh.Attach(ctx, ssh.Options{Binary: bin}, target(t, "box"), tmuxArgv, ssh.Stdio{}); err == nil {
		t.Error("a cancelled context started ssh")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("ssh ran under a cancelled context")
	}
}

// Nothing that could be read as an option, a command or a second destination
// gets to the exec.
func TestAttach_InjectionNeverReachesTheExec(t *testing.T) {
	for _, text := range []string{
		"-oProxyCommand=touch pwned", "u@-oProxyCommand=x", "-u@host", "u@host;id", "u@host'x", "u@$(id)", "u@host name",
		"u@ho`id`st", "u@[fe80::1%en0]", "u@host:22; id", "u@[::1]x", "u@host\nProxyCommand x",
	} {
		bin, log := fakeSSH(t, "0")
		tg, err := domain.ParseSSHTarget(text)
		if err == nil {
			t.Errorf("%q parsed as %+v", text, tg)
			continue
		}
		if _, err := ssh.Attach(t.Context(), ssh.Options{Binary: bin}, tg, tmuxArgv, ssh.Stdio{}); !errors.Is(err, ssh.ErrRejected) {
			t.Errorf("%q: the zero target was not refused: %v", text, err)
		}
		if _, statErr := os.Stat(log); statErr == nil {
			t.Errorf("%q reached ssh", text)
		}
	}
	// A target built by hand is checked as a parsed one is.
	for _, hand := range []domain.SSHTarget{
		{Host: "-oProxyCommand=x"}, {User: "a;b", Host: "h"}, {Host: "h", Port: 70000}, {Host: "h", Port: -1}, {Host: "h w"}, {},
	} {
		bin, log := fakeSSH(t, "0")
		if _, err := ssh.Attach(t.Context(), ssh.Options{Binary: bin}, hand, tmuxArgv, ssh.Stdio{}); !errors.Is(err, ssh.ErrRejected) {
			t.Errorf("%+v: %v", hand, err)
		}
		if _, statErr := os.Stat(log); statErr == nil {
			t.Errorf("%+v reached ssh", hand)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "$(id)", "`id`", "a'b", `a"b`, "a\nb", "a|b", "a&b", "a>b", "a*b", "~", "a\\b", "ü", "%s", "a,b", "a@b"} {
		bin, log := fakeSSH(t, "0")
		if _, err := ssh.Attach(t.Context(), ssh.Options{Binary: bin}, target(t, "box"), append(slices.Clone(tmuxArgv), bad), ssh.Stdio{}); !errors.Is(err, ssh.ErrRejected) {
			t.Errorf("remote element %q: %v", bad, err)
		}
		if _, statErr := os.Stat(log); statErr == nil {
			t.Errorf("remote element %q reached ssh", bad)
		}
	}
	if _, err := ssh.Command(nil); !errors.Is(err, ssh.ErrRejected) {
		t.Errorf("an empty command: %v", err)
	}
}

// The remote shell reads the command back as the same words: run it with sh,
// as the remote side does.
func TestCommand_ASHReadsBackTheSameWords(t *testing.T) {
	argv := []string{"/bin/echo", "a=b:c", "+x-y_z.w/1"}
	command, err := ssh.Command(argv)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", command).Output() //nolint:gosec // a quoted command of our own
	if err != nil || string(out) != "a=b:c +x-y_z.w/1\n" {
		t.Fatalf("sh -c %s = %q, %v", command, out, err)
	}
}

// The interrupt of the terminal reaches ssh with the rest of the foreground
// group, and the context of the client ends with that same interrupt: ending the
// context must not end the session.
func TestAttach_CancellingTheContextAfterItStartedDoesNotKillSSH(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ssh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.4\nexit 3\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	code, err := ssh.Attach(ctx, ssh.Options{Binary: bin}, target(t, "box"), tmuxArgv, ssh.Stdio{})
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v: ssh was cut by the context", code, err)
	}
}

// ssh itself reads what Argv gives it as the target says (ssh -G prints the
// configuration it would use, without connecting).
func TestArgv_SSHReadsTheDestinationAndPortAsIntended(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	for text, want := range map[string]map[string]string{
		"alex@mini.lan:2222": {"user": "alex", "hostname": "mini.lan", "port": "2222"},
		"u@[::1]":            {"user": "u", "hostname": "::1"},
		"u@[fe80::1]:2200":   {"user": "u", "hostname": "fe80::1", "port": "2200"},
		"10.0.0.7":           {"hostname": "10.0.0.7"},
	} {
		args, err := ssh.Argv(target(t, text), []string{"/usr/bin/tmux"})
		if err != nil {
			t.Fatal(err)
		}
		// The command is the last argument; -G needs the options and the destination only.
		out, err := exec.CommandContext(t.Context(), sshBin, append([]string{"-G"}, args[:len(args)-1]...)...).Output() //nolint:gosec // our own argv
		if err != nil {
			t.Fatalf("%s: ssh -G: %v", text, err)
		}
		got := map[string]string{}
		for _, l := range strings.Split(string(out), "\n") {
			k, v, _ := strings.Cut(l, " ")
			got[k] = v
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: ssh reads %s as %q, want %q", text, k, got[k], v)
			}
		}
		for k, v := range map[string]string{"forwardagent": "no", "forwardx11": "no", "permitlocalcommand": "no"} {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", text, k, got[k], v)
			}
		}
		if len(got["localforward"])+len(got["remoteforward"])+len(got["dynamicforward"]) != 0 {
			t.Errorf("%s: forwards survive: %v", text, got)
		}
	}
}
