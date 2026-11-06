//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// tuiSession is the real `cf` TUI of a box, in a tmux pane big enough for every
// view: the person's keys go in with send-keys and the screen they see comes
// out with capture-pane. Its tmux server lives under the TMUX_TMPDIR of the
// box, so the teardown of the harness ends it.
type tuiSession struct {
	c    *Cluster
	box  *Box
	name string
}

const (
	tuiWidth  = 160
	tuiHeight = 45
)

// openTUI starts bare `cf` in the owner's clone, as a person at that machine would.
func openTUI(c *Cluster) *tuiSession {
	c.t.Helper()
	s := &tuiSession{c: c, box: c.Ctl, name: "tui"}
	s.tmux("new-session", "-d", "-x", fmt.Sprint(tuiWidth), "-y", fmt.Sprint(tuiHeight), "-s", s.name, "-c", c.Repo, binary)
	return s
}

func (s *tuiSession) tmux(args ...string) string {
	s.c.t.Helper()
	cmd := exec.Command("tmux", append([]string{"-f", "/dev/null", "-L", s.name}, args...)...) //nolint:gosec,noctx // tmux returns at once
	cmd.Env = s.box.Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.c.t.Fatalf("e2e: tmux %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// Screen is what the person sees now.
func (s *tuiSession) Screen() string {
	s.c.t.Helper()
	return s.tmux("capture-pane", "-p", "-t", s.name)
}

// Keys presses named keys (tmux spelling: Enter, Escape, C-p).
func (s *tuiSession) Keys(keys ...string) {
	s.c.t.Helper()
	s.tmux(append([]string{"send-keys", "-t", s.name}, keys...)...)
}

// Type types text as characters, none of it a key name of tmux's.
func (s *tuiSession) Type(text string) {
	s.c.t.Helper()
	s.tmux("send-keys", "-t", s.name, "-l", text)
}

// WaitFor waits until the screen shows every text, and returns that screen.
func (s *tuiSession) WaitFor(what string, texts ...string) string {
	s.c.t.Helper()
	var screen string
	s.c.Eventually(what, func() (bool, string) {
		screen = s.Screen()
		for _, t := range texts {
			if !strings.Contains(screen, t) {
				return false, "no " + t + " on:\n" + screen
			}
		}
		return true, screen
	})
	return screen
}

// Alive says whether the TUI is still open in its pane.
func (s *tuiSession) Alive() bool {
	cmd := exec.Command("tmux", "-f", "/dev/null", "-L", s.name, "has-session", "-t", s.name) //nolint:gosec,noctx // tmux returns at once
	cmd.Env = s.box.Env()
	return cmd.Run() == nil
}

// Back presses Escape and waits until gone, a text only the screen being left
// shows, is off the screen. Waiting for that effect, not for any change, matters:
// a feed update also changes the screen, and an Escape followed at once by
// another key reaches the TUI as that key with Alt.
func (s *tuiSession) Back(gone string) {
	s.c.t.Helper()
	s.Keys("Escape")
	s.c.Eventually(gone+" to close on Escape", func() (bool, string) {
		now := s.Screen()
		return !strings.Contains(now, gone), now
	})
}

var cursorLine = regexp.MustCompile(`^\s*› (.*)$`)

// cursorAt is the line of the screen under the cursor of a list, -1 if none.
func cursorAt(screen string) int {
	for i, line := range strings.Split(screen, "\n") {
		if cursorLine.MatchString(line) {
			return i
		}
	}
	return -1
}

// onCursor says whether the row under the cursor is the one of label.
func onCursor(screen, label string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(label) + `\b`)
	for _, line := range strings.Split(screen, "\n") {
		if m := cursorLine.FindStringSubmatch(line); m != nil {
			return re.MatchString(m[1])
		}
	}
	return false
}

// SelectRow moves the cursor of the list in view down to the row of label,
// waiting after each Down for the cursor itself to change line.
func (s *tuiSession) SelectRow(label string) {
	s.c.t.Helper()
	for range 10 {
		screen := s.WaitFor("the list of "+label, label)
		if onCursor(screen, label) {
			return
		}
		from := cursorAt(screen)
		s.Keys("Down")
		s.c.Eventually("the cursor to leave line "+fmt.Sprint(from), func() (bool, string) {
			now := s.Screen()
			return cursorAt(now) != from, now
		})
	}
	s.c.t.Fatalf("e2e: the cursor never reached %q:\n%s", label, s.Screen())
}

// Colon runs a line of the command mode and waits for the screen to say it ran.
func (s *tuiSession) Colon(line string) {
	s.c.t.Helper()
	s.Keys(":")
	s.WaitFor("the command line", "Command line")
	s.Type(line)
	s.WaitFor("the line typed", line[:min(len(line), 20)])
	s.Keys("Enter")
	s.WaitFor("the line to run", "✓ cf ")
	s.Back("Command line")
}
