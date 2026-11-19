//go:build integration && (linux || darwin)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/client"
	"github.com/Autarquico/codeflock/internal/domain"
)

// chatScript is a Run that reads lines until "quit", logging each one in
// typed.txt, and then waits for the go file: a terminal that several people can
// type into, as the relay lets them.
const chatScript = `touch ready
echo hello-from-run
while read line; do
  echo "$line" >> typed.txt
  [ "$line" = quit ] && break
done
n=0
while [ ! -e go ]; do
  n=$((n+1)); [ "$n" -gt 2400 ] && exit 99
  sleep 0.05
done
echo finished-line`

// screen is what the terminal of the person shows.
func (o *outer) screen() string { return o.tmux("capture-pane", "-p", "-t", "0") }

// detach types the key of cf that leaves a relay session.
func (o *outer) detach() { o.send("C-]") }

// typed is what the Run has read.
func (o *observeBox) typed() string { return readFile(filepath.Join(o.worktree, "typed.txt")) }

// finishChat ends the Run the way it asks to end: a line, and the go file.
func (o *observeBox) finishChat() {
	o.t.Helper()
	if _, err := o.workerTmux("send-keys", "-t", "="+o.session+":", "quit", "Enter"); err != nil {
		o.t.Fatal(err)
	}
	o.release(o.run.ID)
	if r := o.waitPublished(o.run.ID); r.State != domain.RunCompleted {
		o.t.Errorf("Run = %+v", r)
	}
}

// ended says whether the terminal of the person is gone: cf exited in it.
func (o *outer) ended() bool { return o.tmux("list-sessions", "-F", "#{session_name}") == "" }

// TestScenario_LiveAttach_AttachSinSSH: with no ssh at all (the one in the PATH
// logs what it is asked, and is never run), `cf attach` opens the session of the
// Run through the Controller: what is typed arrives, the screen of the Run
// comes back, and detaching leaves the Run where it was.
func TestScenario_LiveAttach_AttachSinSSH(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	listed := ok[v1.PsResponse](t, "ps")
	if len(listed.Runs) != 1 || !slices.Contains(listed.Runs[0].Transports, v1.AttachTransportRelay) {
		t.Fatalf("ps = %+v", listed)
	}

	terminal := o.attach(nil)
	eventually(t, "the relay client to be on the session of the Worker", func() bool { return o.clients() == 1 })
	eventually(t, "the screen of the Run", func() bool { return strings.Contains(terminal.screen(), "hello-from-run") })
	terminal.send("typed over the relay", "Enter")
	eventually(t, "the Run to read the line", func() bool { return o.typed() == "typed over the relay" })
	eventually(t, "the echo on the terminal of the person", func() bool {
		return strings.Count(terminal.screen(), "typed over the relay") >= 1
	})
	if got := readFile(o.sshLog); got != "" {
		t.Fatalf("ssh was asked for something:\n%s", got)
	}

	terminal.detach()
	eventually(t, "the client to leave the session", func() bool { return o.clients() == 0 })
	eventually(t, "cf attach to exit", terminal.ended)
	if !o.sessionAlive() || o.reconcileBox.run(o.run.ID).State != domain.RunRunning {
		t.Fatal("detaching ended the Run")
	}
	o.finishChat()
}

// modes is what the terminal of the person is in: its alternate screen, cursor
// and mouse reporting, as the outer tmux tracks them.
func (o *outer) modes() string {
	return o.tmux("display-message", "-p", "-t", "0", "alternate=#{alternate_on} cursor=#{cursor_flag} mouse=#{mouse_any_flag}")
}

// TestScenario_LiveAttach_TerminalRestaurada: leaving with Ctrl-] ends the
// stream before tmux can say anything, so cf itself leaves the terminal as it
// found it: out of the alternate screen, with the cursor and no mouse reporting.
func TestScenario_LiveAttach_TerminalRestaurada(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	exe, _ := os.Executable()
	code := filepath.Join(t.TempDir(), "code")
	term := inTerminal(t, o.attachEnv(), "sh", "-c", fmt.Sprintf("%s attach auth-race; echo $? > %s; sleep 120", exe, code))
	eventually(t, "the screen of the Run", func() bool { return strings.Contains(term.screen(), "hello-from-run") })
	if got := term.modes(); !strings.Contains(got, "alternate=1") {
		t.Fatalf("while attached the terminal is %q: the test would prove nothing", got)
	}
	term.detach()
	eventually(t, "cf attach to end", func() bool { return fileExists(code) })
	if got := term.modes(); got != "alternate=0 cursor=1 mouse=0" {
		t.Errorf("after Ctrl-] the terminal is %q", got)
	}
	// The banner stays and the shell starts under it, not over it.
	lines := strings.Split(strings.TrimRight(term.screen(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "attached to ") {
		t.Errorf("the banner is not on the first line of the terminal:\n%s", term.screen())
	}
	if row := term.tmux("display-message", "-p", "-t", "0", "#{cursor_y}"); strings.TrimSpace(row) == "0" {
		t.Errorf("the cursor is on the first row, over the banner")
	}
	o.finishChat()
}

// TestScenario_LiveAttach_SoloLectura: with --read-only the person sees the
// Run and what they type never reaches it, not even by the detach key's side.
func TestScenario_LiveAttach_SoloLectura(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	view := o.attach(nil, "--read-only")
	eventually(t, "the view to be on the session", func() bool { return o.clients() == 1 })
	eventually(t, "the screen of the Run", func() bool { return strings.Contains(view.screen(), "hello-from-run") })
	view.send("nope", "Enter")
	view.detach()
	eventually(t, "the view to leave", func() bool { return o.clients() == 0 })
	eventually(t, "cf attach to exit", view.ended)

	// A writer after it does reach the Run: the Worker was never wedged.
	writer := o.attach(nil)
	eventually(t, "the writer", func() bool { return o.clients() == 1 })
	writer.send("yes", "Enter")
	// The Run reads in order: that what it has read is exactly "yes" is the proof
	// that "nope", typed first, never reached it.
	eventually(t, "the input to arrive", func() bool { return o.typed() == "yes" })
	writer.detach()
	o.finishChat()
}

// TestScenario_LiveAttach_DosEscritores: two people in the Run, both type, the
// Run reads both and each sees the other on their screen; the peers each client
// gets name both.
func TestScenario_LiveAttach_DosEscritores(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	a, b := o.attach(nil), o.attach(nil)
	eventually(t, "both clients to be on the session", func() bool { return o.clients() == 2 })
	a.send("from-a", "Enter")
	eventually(t, "the line of a", func() bool { return strings.Contains(o.typed(), "from-a") })
	b.send("from-b", "Enter")
	eventually(t, "the line of b", func() bool { return strings.Contains(o.typed(), "from-b") })
	if got := strings.Fields(o.typed()); !slices.Equal(got, []string{"from-a", "from-b"}) {
		t.Fatalf("the Run read %q", got)
	}
	eventually(t, "a to see b", func() bool { return strings.Contains(a.screen(), "from-b") })
	eventually(t, "b to see a", func() bool { return strings.Contains(b.screen(), "from-a") })
	a.detach()
	b.detach()
	eventually(t, "both to leave", func() bool { return o.clients() == 0 })

	// At the level of the client: the Controller tells each one who is in the Run.
	c, err := client.Open(o.client.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.OpenAttach(t.Context(), o.run.ID, "", v1.AttachModeWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if got := nextPeers(t, first); len(got) != 1 || got[0].Mode != v1.AttachModeWrite {
		t.Fatalf("alone: %+v", got)
	}
	second, err := c.OpenAttach(t.Context(), o.run.ID, "", v1.AttachModeView)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	for _, conn := range []*client.AttachConn{first, second} {
		var modes []string
		for _, p := range awaitPeers(t, conn, 2) {
			modes = append(modes, p.Mode)
		}
		slices.Sort(modes)
		if !slices.Equal(modes, []string{v1.AttachModeView, v1.AttachModeWrite}) {
			t.Fatalf("peers of both = %v", modes)
		}
	}
	_ = first.Detach()
	_ = second.Detach()
	eventually(t, "the clients to leave", func() bool { return o.clients() == 0 })
	o.finishChat()
}

// nextPeers reads frames until a list of peers comes.
func nextPeers(t *testing.T, c *client.AttachConn) []v1.AttachPeer {
	t.Helper()
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for peers: %v", err)
		}
		switch f.Type {
		case v1.FramePeers:
			peers, err := v1.ParsePeers(f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			return peers
		case v1.FrameError:
			t.Fatalf("the stream was cut: %v", client.StreamError(f))
		}
	}
}

// awaitPeers reads until a list of n peers comes.
func awaitPeers(t *testing.T, c *client.AttachConn, n int) []v1.AttachPeer {
	t.Helper()
	for {
		if peers := nextPeers(t, c); len(peers) == n {
			return peers
		}
	}
}

// TestScenario_LiveAttach_WorkerQueCae: the Worker is killed while a person is
// attached: they see the cut with CF_WORKER_OFFLINE and cf exits; once the
// Worker is back they can attach again.
func TestScenario_LiveAttach_WorkerQueCae(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	exe, _ := os.Executable()

	attach := func(name string) (*outer, string) {
		out := filepath.Join(t.TempDir(), name)
		term := inTerminal(t, o.attachEnv(), "sh", "-c", fmt.Sprintf("%s attach auth-race 2> %s.err; echo $? > %s.code; sleep 120", exe, out, out))
		return term, out
	}
	first, firstOut := attach("first")
	eventually(t, "the client to be on the session", func() bool { return o.clients() == 1 })
	eventually(t, "the screen of the Run", func() bool { return strings.Contains(first.screen(), "hello-from-run") })

	o.daemon.kill()
	eventually(t, "cf attach to end", func() bool { return fileExists(firstOut + ".code") })
	if got, want := readFile(firstOut+".code"), fmt.Sprint(domain.ExitCode(domain.CodeWorkerOffline)); got != want || !strings.Contains(readFile(firstOut+".err"), string(domain.CodeWorkerOffline)) {
		t.Fatalf("exit %s (want %s), stderr %q", got, want, readFile(firstOut+".err"))
	}
	if got := first.modes(); got != "alternate=0 cursor=1 mouse=0" {
		t.Errorf("after the cut the terminal is %q", got)
	}
	eventually(t, "the tmux client to be gone", func() bool { return o.clients() == 0 })
	if !o.sessionAlive() {
		t.Fatal("the Run's session died with the Worker's connection")
	}

	// The Worker comes back, and the person can go in again (until its first poll is in, the Controller says it is offline).
	o.startWith("--ssh-target", "tester@fake-worker:2222")
	var again *outer
	eventually(t, "attaching again to work", func() bool {
		term, out := attach(fmt.Sprint("again-", time.Now().UnixNano()))
		for range 40 {
			if o.clients() == 1 {
				again = term
				return true
			}
			if fileExists(out + ".code") {
				term.kill()
				return false
			}
			time.Sleep(50 * time.Millisecond)
		}
		term.kill()
		return false
	})
	again.send("back", "Enter")
	eventually(t, "the Run to read the line", func() bool { return o.typed() == "back" })
	again.detach()
	o.finishChat()
}

// TestScenario_LiveAttach_SinRelayEnElWorker: a Worker that does not announce
// attach-relay:v1 offers ssh and nothing else. Asking for the relay is refused
// with CF_RUN_NOT_ATTACHABLE (relay_unsupported) before anything is run, and
// ssh is not a silent fallback; with --ssh the session opens as it always did.
func TestScenario_LiveAttach_SinRelayEnElWorker(t *testing.T) {
	o := newObserveBox(t, holdScript, "--no-attach-relay")
	use(t, o.client)

	// cf ps says how the Run can be attached.
	listed := ok[v1.PsResponse](t, "ps")
	if len(listed.Runs) != 1 || !listed.Runs[0].Attachable || !slices.Equal(listed.Runs[0].Transports, []string{v1.AttachTransportSSH}) {
		t.Fatalf("ps = %+v", listed)
	}
	if code, out, _ := cf(t, "", "ps"); code != 0 || !strings.Contains(out, "TRANSPORT") {
		t.Fatalf("ps table: exit %d\n%s", code, out)
	}

	// The relay is refused, and ssh is not started in its place.
	t.Setenv("PATH", o.fakeBin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_LOG", o.sshLog)
	term := &commandTerminal{}
	_, err := cli.PrepareAttach(t.Context(), term, cli.AttachParams{Ref: "auth-race", Transport: cli.TransportRelay})
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeRunNotAttachable || coded.Details["reason"] != v1.NotAttachableRelayUnsupported ||
		!strings.Contains(coded.Message, "--ssh") {
		t.Fatalf("relay without the capability: %v", err)
	}
	if term.calls.Load() != 0 || readFile(o.sshLog) != "" {
		t.Fatalf("the terminal was reached %d times, ssh log %q", term.calls.Load(), readFile(o.sshLog))
	}

	// The command says so too, with the status of a Run that cannot be attached, and no ssh.
	refused := filepath.Join(t.TempDir(), "refused")
	exe, _ := os.Executable()
	inTerminal(t, o.attachEnv(), "sh", "-c", fmt.Sprintf("%s attach auth-race --json > %s; echo $? > %s.code", exe, refused, refused))
	eventually(t, "cf attach to end", func() bool { return fileExists(refused + ".code") })
	if got := readFile(refused + ".code"); got != "8" || !strings.Contains(readFile(refused), v1.NotAttachableRelayUnsupported) || readFile(o.sshLog) != "" {
		t.Fatalf("exit %s, output %s, ssh log %q", got, readFile(refused), readFile(o.sshLog))
	}

	// With --ssh the session opens as today.
	terminal := o.attach(nil, "--ssh")
	eventually(t, "the attach client to be on the session of the Worker", func() bool { return o.clients() == 1 })
	if !strings.Contains(readFile(o.sshLog), "tester@fake-worker") {
		t.Fatalf("ssh was not run:\n%s", readFile(o.sshLog))
	}
	terminal.kill()
	eventually(t, "the client to leave the session", func() bool { return o.clients() == 0 })
	if _, err := o.workerTmux("send-keys", "-t", "="+o.session+":", "go on", "Enter"); err != nil { // the harness waits for a line
		t.Fatal(err)
	}
	o.release(o.run.ID)
	o.waitPublished(o.run.ID)
}

// useDetachKeyCtrlG sets a detach key for cf that is not Ctrl-], so that the
// Ctrl-] a person types is the fallback bound in the tmux of the Worker.
func useDetachKeyCtrlG(t *testing.T, o *observeBox) {
	t.Helper()
	config := filepath.Join(o.client.ConfigDir, "client.json")
	if err := os.WriteFile(config, []byte(`{"attach":{"detach_key":"ctrl+g"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestScenario_LiveAttach_TeclaDeRespaldoAlEntrar: with a detach key of cf that
// is not Ctrl-], a Ctrl-] already waiting in the terminal when cf starts is read
// the moment the terminal is raw, goes to the Worker with the attach opening, and
// still leaves through the fallback bound in the tmux of the Worker.
//
// Not covered: the hold of the relay. This test also passes with the hold
// disabled (checked by hand), so it does not guard it;
// TestRelay_InputBeforeTheTerminalDrawsWaitsAndArrivesInOrder does.
func TestScenario_LiveAttach_TeclaDeRespaldoAlEntrar(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	useDetachKeyCtrlG(t, o)
	exe, _ := os.Executable()
	term := inTerminal(t, o.attachEnv(), "sh", "-c", fmt.Sprintf("sleep 1; exec %s attach auth-race", exe))
	term.send("C-]")
	eventually(t, "cf attach to exit", term.ended)
	eventually(t, "the client to leave the session", func() bool { return o.clients() == 0 })
	if !o.sessionAlive() || o.reconcileBox.run(o.run.ID).State != domain.RunRunning {
		t.Fatal("leaving ended the Run")
	}
	o.finishChat()
}
