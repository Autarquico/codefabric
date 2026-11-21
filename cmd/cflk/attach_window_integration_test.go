//go:build integration && (linux || darwin)

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
)

// windows are the names of the windows of the tmux server of the person.
func (o *outer) windows() []string {
	return strings.Fields(o.tmux("list-windows", "-F", "#{window_name}"))
}

// windowScreen is what the window at index shows.
func (o *outer) windowScreen(index int) string {
	return o.tmux("capture-pane", "-p", "-t", fmt.Sprint(index))
}

// attachWithoutTTY runs `cflk attach` in a pane of the person's tmux (so TMUX is
// set) with /dev/null as its input and the given args, and returns where its
// stdout and exit status land.
func attachWithoutTTY(t *testing.T, o *observeBox, args string) (term *outer, out string) {
	t.Helper()
	exe, _ := os.Executable()
	out = filepath.Join(t.TempDir(), "out")
	term = inTerminal(t, o.attachEnv(), "sh", "-c", fmt.Sprintf("%s attach %s < /dev/null > %s; echo $? > %s.code; sleep 120", exe, args, out, out))
	eventually(t, "cflk attach to end", func() bool { return fileExists(out + ".code") })
	return term, out
}

// TestScenario_AttachWindow_TmuxReal: without a terminal on its input, inside
// tmux, cflk attach opens a window of tmux that has a terminal and is the real
// attach, with the Run already resolved; the Run goes on when it is detached.
func TestScenario_AttachWindow_TmuxReal(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term, out := attachWithoutTTY(t, o, "auth-race")
	if got := readFile(out + ".code"); got != "0" {
		t.Fatalf("exit %s", got)
	}
	if got, want := readFile(out), "Opened "+o.run.Short+" in a new tmux window"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := term.windows(); len(got) != 2 || got[1] != o.run.Short {
		t.Fatalf("windows = %q", got)
	}
	eventually(t, "the screen of the Run in the new window", func() bool { return strings.Contains(term.windowScreen(1), "hello-from-run") })
	eventually(t, "the relay client to be on the session", func() bool { return o.clients() == 1 })

	term.tmux("send-keys", "-t", "1", "C-]")
	eventually(t, "the window to end", func() bool { return len(term.windows()) == 1 })
	if !o.sessionAlive() || o.reconcileBox.run(o.run.ID).State != domain.RunRunning {
		t.Fatal("detaching ended the Run")
	}
	o.finishChat()
}

// TestScenario_AttachWindow_JSON: the answer of --json names the launcher and the Run.
func TestScenario_AttachWindow_JSON(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	_, out := attachWithoutTTY(t, o, "auth-race --json")
	var got map[string]string
	if err := json.Unmarshal([]byte(readFile(out)), &got); err != nil || got["opened"] != "tmux" || got["run"] != o.run.ID || len(got) != 2 {
		t.Errorf("stdout = %q (%v)", readFile(out), err)
	}
}

// TestScenario_AttachWindow_ElRunNoSePuedeAbrir: a Run that cannot be attached
// gives its usual error and opens no window.
func TestScenario_AttachWindow_ElRunNoSePuedeAbrir(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term, out := attachWithoutTTY(t, o, "no-such-task")
	if got, want := readFile(out+".code"), fmt.Sprint(domain.ExitCode(domain.CodeNotFound)); got != want {
		t.Errorf("exit %s, want %s", got, want)
	}
	if got := term.windows(); len(got) != 1 {
		t.Errorf("windows = %q", got)
	}
}

// TestScenario_AttachWindow_ConTerminalNadaCambia: with a terminal the attach
// is the one of now, in the same window.
func TestScenario_AttachWindow_ConTerminalNadaCambia(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term := o.attach(nil)
	eventually(t, "the screen of the Run in the same window", func() bool { return strings.Contains(term.screen(), "hello-from-run") })
	if got := term.windows(); len(got) != 1 {
		t.Errorf("windows = %q", got)
	}
	term.detach()
	eventually(t, "cflk attach to exit", term.ended)
	o.finishChat()
}
