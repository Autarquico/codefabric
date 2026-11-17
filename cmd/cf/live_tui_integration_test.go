//go:build integration && (linux || darwin)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The live view of the TUI against the real Controller, the real relay of the
// Worker and real tmux. The TUI is the real one: bare `cf` running in a pane of
// an outer tmux of the test's own, which plays the terminal of the person (and
// is the "tmux of fuera" of the spec). Its keys are sent with tmux send-keys and
// what it shows is read with capture-pane.
//
// What these do not cover: one Worker serves every Run (the box has a single
// Worker), so "Runs on different Workers" is three Runs on one; the cut of the
// stream when the Worker drops, with the TUI in front, is left to
// TestScenario_LiveAttach_WorkerQueCae (the same stream, with cf attach) and to
// the unit tests of the panel's reason lines.

// tui opens the TUI in a pane of an outer tmux. The window is 200x50.
func (o *observeBox) tui(extraEnv ...string) *outer {
	exe, _ := os.Executable()
	return inTerminal(o.t, o.attachEnv(extraEnv...), exe)
}

// within fails if the text is not on the screen within d: the panel shows what
// a Run wrote in under a second.
func within(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, d)
}

// TestScenario_LiveAttach_VistaEnDirecto: the TUI in the Tasks view with a
// running interactive Run selected; the agent writes in its terminal, and the
// panel shows it in under a second, without leaving the TUI, and read-only: what
// is typed in the TUI is not typed into the Run.
func TestScenario_LiveAttach_VistaEnDirecto(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	term := o.tui()
	eventually(t, "the home", func() bool { return strings.Contains(term.screen(), "CODEFABRIC") })
	term.send("t")
	eventually(t, "the Task", func() bool { return strings.Contains(term.screen(), "auth-race") })
	term.send("v")
	eventually(t, "the panel with the screen of the Run", func() bool {
		s := term.screen()
		return strings.Contains(s, "Live ·") && strings.Contains(s, "hello-from-run")
	})
	if !o.sessionAlive() || o.clients() != 0 {
		t.Fatalf("the panel is not a tmux client of the Run (clients: %d)", o.clients())
	}

	typeInto(t, o, o.session, "marker-one")
	began := time.Now()
	within(t, time.Second, "the text in the panel", func() bool { return strings.Contains(term.screen(), "marker-one") })
	t.Logf("the text took %s to show in the panel", time.Since(began))

	// Read-only: keys of the TUI do not reach the Run.
	term.send("hello-tui")
	time.Sleep(300 * time.Millisecond)
	if got := o.typed(); strings.Contains(got, "hello-tui") {
		t.Fatalf("the panel is not read-only: the Run read %q", got)
	}
	term.send("q")
	o.finishChat()
}

// TestScenario_LiveAttach_VariosALaVez: three live Runs, the grid of the TUI
// shows the three at once and live. (They share the single Worker of the box.)
func TestScenario_LiveAttach_VariosALaVez(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	runs := []v1.Run{o.run}
	for _, name := range []string{"second", "third"} {
		created := ok[v1.CreateTaskResponse](t, "do", chatScript, "--harness", "command", "-i", "--name", name)
		o.waitRunning(created.Run.ID)
		runs = append(runs, *created.Run)
	}
	term := o.tui()
	eventually(t, "the home", func() bool { return strings.Contains(term.screen(), "CODEFABRIC") })
	term.send("t")
	eventually(t, "the three Tasks", func() bool {
		s := term.screen()
		return strings.Contains(s, "auth-race") && strings.Contains(s, "second") && strings.Contains(s, "third")
	})
	term.send("V")
	eventually(t, "the three cells", func() bool {
		s := term.screen()
		return strings.Contains(s, "Grid · 3 live") && strings.Count(s, "hello-from-run") == 3
	})
	// Live: each one shows what its own Run is given.
	for _, r := range runs {
		typeInto(t, o, domain.SessionName(r.ID, 1), "only-in-"+r.ID[len(r.ID)-6:])
	}
	for _, r := range runs {
		mark := "only-in-" + r.ID[len(r.ID)-6:]
		within(t, 3*time.Second, mark+" in its cell", func() bool { return strings.Contains(term.screen(), mark) })
	}
	term.send("Escape")
	eventually(t, "the Tasks view back", func() bool { return !strings.Contains(term.screen(), "Grid ·") })
	term.send("q")
	for _, r := range runs {
		o.release(r.ID)
		typeInto(t, o, domain.SessionName(r.ID, 1), "quit")
	}
}

// outerState is the options and key bindings of the tmux of the person, which
// the TUI and the attach must leave as they found them.
func (o *outer) state() string {
	var parts []string
	for _, args := range [][]string{{"show-options", "-g"}, {"show-options", "-s"}, {"show-window-options", "-g"}, {"list-keys"}} {
		parts = append(parts, o.tmux(args...))
	}
	return strings.Join(parts, "\n--\n")
}

// listOf is what the TUI shows above the pane under the list: the part of the
// screen that must be the same when the person comes back.
func listOf(screen string) string {
	var out []string
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, " Live ") {
			break
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	return strings.Join(out, "\n")
}

// TestScenario_LiveAttach_EntrarYSalir: the TUI inside tmux, the panel of a Run
// on, enter takes the Run full screen, what is typed reaches it, the key of cf
// from client.json (not tmux's) brings the person back to the same place in the
// TUI, the Run is still running, and the options and key bindings of the outer
// tmux are as they were.
//
// Not covered: that the person's tmux does not see the detach key. The test
// sends the key to that same tmux, which is what a person does, and the proof
// that it works is that the TUI is back with the outer tmux session alive and
// unchanged.
func TestScenario_LiveAttach_EntrarYSalir(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	// A key that is not the default and is not C-b: the one that is in the config wins.
	config := filepath.Join(o.client.ConfigDir, "client.json")
	if err := os.WriteFile(config, []byte(`{"attach":{"detach_key":"ctrl+g"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	term := o.tui()
	eventually(t, "the home", func() bool { return strings.Contains(term.screen(), "CODEFABRIC") })
	term.send("t")
	eventually(t, "the Task", func() bool { return strings.Contains(term.screen(), "auth-race") })
	term.send("v")
	eventually(t, "the panel", func() bool { return strings.Contains(term.screen(), "hello-from-run") })
	eventually(t, "the key to leave, as configured", func() bool { return strings.Contains(term.screen(), "Ctrl-G leaves") })
	before := listOf(term.screen())
	outerBefore := term.state()

	term.send("Enter")
	eventually(t, "the attach", func() bool { return o.clients() == 1 })
	eventually(t, "the Run on the whole screen", func() bool {
		s := term.screen()
		return strings.Contains(s, "hello-from-run") && !strings.Contains(s, "Live ·")
	})
	term.send("typed-from-the-tui", "Enter")
	eventually(t, "the Run to read the line", func() bool { return o.typed() == "typed-from-the-tui" })

	term.send("C-g")
	eventually(t, "the client to leave the Run", func() bool { return o.clients() == 0 })
	eventually(t, "the TUI back with its panel", func() bool {
		s := term.screen()
		return strings.Contains(s, "Live ·") && strings.Contains(s, "typed-from-the-tui") && strings.Contains(s, "Back from")
	})
	term.send("z") // clears the line of "Back from"
	eventually(t, "the same list", func() bool {
		return listOf(term.screen()) == before || strings.TrimSpace(strings.TrimPrefix(listOf(term.screen()), before)) == ""
	})
	if got := listOf(term.screen()); got != before {
		t.Errorf("the TUI came back to a different place:\n--- before\n%s\n--- after\n%s", before, got)
	}
	if r := o.reconcileBox.run(o.run.ID); r.State != domain.RunRunning || !o.sessionAlive() {
		t.Fatalf("leaving ended the Run: %+v", r)
	}
	if after := term.state(); after != outerBefore {
		t.Errorf("the outer tmux changed:\n--- before\n%s\n--- after\n%s", outerBefore, after)
	}
	if term.ended() {
		t.Fatal("the outer tmux session is gone")
	}
	term.send("q")
	o.finishChat()
}

// TestScenario_LiveAttach_DosEscritoresYPanel: two writers are inside the Run
// and the TUI of a third terminal lists both in the title of its panel, and shows
// what each types. (Both are the same enrolled client, "laptop", so the panel
// names it with how many times it is inside: the box enrolls one client.)
//
// Not covered: that the two writers are told apart by name. The box enrolls one
// client, so both are "laptop", and the assertion is on the count (×2), which
// is what the panel says of a client that is inside twice.
func TestScenario_LiveAttach_DosEscritoresYPanel(t *testing.T) {
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	a, b := o.attach(nil), o.attach(nil)
	eventually(t, "both writers", func() bool { return o.clients() == 2 })

	term := o.tui()
	eventually(t, "the home", func() bool { return strings.Contains(term.screen(), "CODEFABRIC") })
	term.send("t")
	eventually(t, "the Task", func() bool { return strings.Contains(term.screen(), "auth-race") })
	term.send("v")
	eventually(t, "both listed", func() bool { return strings.Contains(term.screen(), "inside: laptop ×2") })

	a.send("from-a", "Enter")
	b.send("from-b", "Enter")
	eventually(t, "what both typed in the panel", func() bool {
		s := term.screen()
		return strings.Contains(s, "from-a") && strings.Contains(s, "from-b")
	})
	if got := strings.Fields(o.typed()); len(got) != 2 {
		t.Fatalf("the Run read %q", got)
	}
	// One leaves: the list follows.
	b.detach()
	eventually(t, "one left", func() bool {
		return strings.Contains(term.screen(), "inside: laptop") && !strings.Contains(term.screen(), "×2")
	})
	a.detach()
	eventually(t, "both to leave", func() bool { return o.clients() == 0 })
	term.send("q")
	o.finishChat()
}
