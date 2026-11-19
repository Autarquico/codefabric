//go:build integration && (linux || darwin)

package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// requireTmux32 skips the test where tmux is older than 3.2: before it a
// read-only client has no way to leave the size of the window alone.
func requireTmux32(t *testing.T) {
	t.Helper()
	requireTmux(t)
	out, err := exec.CommandContext(t.Context(), "tmux", "-V").Output()
	if err != nil {
		t.Fatalf("tmux -V: %v", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "tmux %d.%d", &major, &minor); err != nil {
		t.Skipf("tmux version %q is not one this test can read", out)
	}
	if major < 3 || (major == 3 && minor < 2) {
		t.Skipf("tmux %d.%d is older than 3.2: a read-only client still follows the size of its terminal there", major, minor)
	}
}

// resizeTerminal gives the terminal of the person a size, as a window of it being dragged would.
func (o *outer) resizeTerminal(cols, rows int) {
	o.tmux("resize-window", "-t", "0", "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows))
}

// size is the size of the pane cf runs in: the terminal cf reads its size from.
func (o *outer) size() string {
	return o.tmux("display-message", "-p", "-t", "0", "#{pane_width}x#{pane_height}")
}

// windowSize is the size of the window of the session on the Worker.
func (o *observeBox) windowSize() string {
	out, _ := o.workerTmux("display-message", "-p", "-t", "="+o.session+":", "#{window_width}x#{window_height}")
	return out
}

// TestScenario_ReadOnlyAttach_VeLaSesionEntera: a view that is smaller than the
// writer sees the session at its own size, and once it is as large it sees all
// of it; neither of its sizes ever changes the window of the writer, and what it
// types does not reach the Run.
func TestScenario_ReadOnlyAttach_VeLaSesionEntera(t *testing.T) {
	requireTmux32(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	writer := o.attach(nil)
	writer.resizeTerminal(160, 45)
	eventually(t, "the writer to be on the session", func() bool { return o.clients() == 1 })
	// The window is the terminal of the writer less the row of tmux's status line.
	const windowOfWriter = "160x44"
	eventually(t, "the window to be the size of the writer", func() bool { return o.windowSize() == windowOfWriter })

	view := o.attach(nil, "--read-only")
	view.resizeTerminal(100, 30)
	eventually(t, "the view to be on the session", func() bool { return o.clients() == 2 })
	eventually(t, "the screen of the Run", func() bool { return strings.Contains(view.screen(), "hello-from-run") })
	if got := o.windowSize(); got != windowOfWriter {
		t.Errorf("a view of 100x30 left the window at %s, not %s", got, windowOfWriter)
	}

	// Once as large as the window, and then larger than it: the Worker gets each
	// size, which the row where tmux draws its status line proves, and the window
	// stays the writer's.
	for _, size := range []struct{ cols, rows int }{{160, 45}, {200, 60}} {
		view.resizeTerminal(size.cols, size.rows)
		eventually(t, fmt.Sprintf("the view to draw the %d rows of its terminal", size.rows), func() bool {
			return view.size() == fmt.Sprintf("%dx%d", size.cols, size.rows) &&
				strings.Contains(view.tmux("capture-pane", "-p", "-t", "0", "-S", fmt.Sprint(size.rows-1), "-E", fmt.Sprint(size.rows-1)), "[cf-run")
		})
		if got := o.windowSize(); got != windowOfWriter {
			t.Errorf("a view of %dx%d left the window at %s, not %s", size.cols, size.rows, got, windowOfWriter)
		}
	}

	view.send("nope", "Enter")
	view.detach()
	eventually(t, "the view to leave", func() bool { return o.clients() == 1 })
	writer.send("yes", "Enter")
	// The Run reads in order: "nope" typed first would be read before "yes".
	eventually(t, "the input to arrive", func() bool { return o.typed() == "yes" })
	writer.detach()
	o.finishChat()
}
