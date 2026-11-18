package tui

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	supscreen "github.com/Autarquico/codefabric/internal/support/screen"
)

// The live view (spec live-attach): the panel of the Tasks and Runs views and
// the cells of the grid are the same thing, a liveFeed: one read-only screen
// stream of a Run, followed one screen at a time by commands so that Update
// never waits for the network.

// Rates of the screen streams, in screens a second. The Worker captures the
// pane of tmux for each one, so a cell of the grid asks for less than the
// panel the person is looking at.
const (
	panelHz = 2
	gridHz  = 1
)

// ScreenStream is the read-only stream of the screen of a Run that the live
// view follows: *client.ScreenStream is one. Close ends it and frees a Next
// that is waiting.
type ScreenStream interface {
	// Next waits for the latest screen: raw bytes of a Worker, SGR included.
	Next() ([]byte, error)
	// Peers is who the Controller last said is inside the Run.
	Peers() []v1.AttachPeer
	Close() error
}

// livePrefs is what the TUI remembers of the live view for its whole session,
// shared by the views that have it: whether the panel is on.
type livePrefs struct {
	panel bool
	// leave is how the key that leaves a session reads ("Ctrl-]"), or "" when
	// it is not known.
	leave string
}

// leaveKey is how the key that leaves a session reads, "" when not known.
func (p *livePrefs) leaveKey() string {
	if p == nil {
		return ""
	}
	return p.leave
}

// enterRun is the label of the key that enters a Run: it says how to leave,
// because once inside the keys go to the Run, not to the TUI.
func (p *livePrefs) enterRun() string {
	if p == nil || p.leave == "" {
		return "Enter run"
	}
	return "Enter run · " + p.leave + " leaves"
}

// Messages of a stream in flight. gen says which opening they belong to: what
// arrives for a generation that was let go of is dropped (and a stream that
// opens late is closed), so nothing outlives the selection that wanted it.
type (
	screenOpenedMsg struct {
		from
		run    string
		gen    uint64
		stream ScreenStream
		err    error
	}
	screenFrameMsg struct {
		from
		run    string
		gen    uint64
		screen []byte
		peers  []v1.AttachPeer
		err    error
	}
)

var lastLiveGen atomic.Uint64

// liveFeed is the stream of one Run, as a screen of the TUI holds it. Its zero
// value follows nothing.
type liveFeed struct {
	run    string // the Run it follows; "" when it follows nothing
	gen    uint64
	stream ScreenStream
	cancel context.CancelFunc // ends the opening of the stream, and the stream with it
	// lines are the sanitized screen, ready to be cropped; peers are the names
	// of who else is inside.
	lines  []string
	peers  []string
	waited bool   // a screen has arrived
	reason string // why there is no screen: one line, empty while there is one or one is coming
	retry  bool   // whether trying again can change the reason
}

// following says whether the feed has a stream, or is opening one, for run.
func (f *liveFeed) following(run string) bool { return f.run == run && f.gen != 0 }

// open starts following run, and ends what it followed before. Nothing is
// asked of the network here: the command that returns does it. The opening
// runs under a context that close cancels, so an opening for a selection that
// was left is cut short instead of holding a slot of the Worker.
func (f *liveFeed) open(backend Backend, asker from, run string, hz int) tea.Cmd {
	f.close()
	gen := lastLiveGen.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	*f = liveFeed{run: run, gen: gen, cancel: cancel}
	return func() tea.Msg {
		stream, err := backend.OpenScreen(ctx, run, hz)
		return screenOpenedMsg{from: asker, run: run, gen: gen, stream: stream, err: err}
	}
}

// close lets go of the stream. A command waiting in its Next is freed by it and
// answers for a generation that is no longer the feed's.
func (f *liveFeed) close() {
	if f.cancel != nil {
		f.cancel()
	}
	if f.stream != nil {
		_ = f.stream.Close()
	}
	*f = liveFeed{}
}

// fail ends the stream and leaves a reason standing: the panel says why, and
// the feed still follows the Run, so nothing opens it again until the selection
// changes or the person retries.
func (f *liveFeed) fail(err error) {
	reason, retry := liveReason(err), retryable(err)
	if f.stream != nil {
		_ = f.stream.Close()
	}
	f.stream, f.lines, f.peers, f.waited, f.reason, f.retry = nil, nil, nil, false, reason, retry
}

// next is the command that waits for the next screen of the stream.
func (f *liveFeed) next(asker from) tea.Cmd {
	stream, run, gen := f.stream, f.run, f.gen
	return func() tea.Msg {
		screen, err := stream.Next()
		return screenFrameMsg{from: asker, run: run, gen: gen, screen: screen, peers: stream.Peers(), err: err}
	}
}

// handle folds a message of a stream into the feed. What is not about this
// feed's generation is dropped, and a stream that opens for a feed that let go
// of it is closed here.
func (f *liveFeed) handle(asker from, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case screenOpenedMsg:
		if f.gen != msg.gen {
			if msg.stream != nil {
				_ = msg.stream.Close()
			}
			return nil
		}
		if msg.err != nil {
			f.fail(msg.err)
			return nil
		}
		f.stream = msg.stream
		return f.next(asker)
	case screenFrameMsg:
		if f.gen != msg.gen || f.stream == nil {
			return nil
		}
		if msg.err != nil {
			f.fail(msg.err)
			return nil
		}
		f.lines, f.waited, f.reason = screenLines(msg.screen), true, ""
		f.peers = peerNames(msg.peers)
		return f.next(asker)
	}
	return nil
}

// screenLines is a captured screen made safe to draw: the Worker's bytes are
// those of a process the person does not control, so everything but colours
// and text is removed (supscreen.Sanitize) before any of it is split into lines
// and drawn. Trailing blank lines are dropped: a pane is captured to its full
// height, and what matters is above them.
func screenLines(raw []byte) []string {
	lines := strings.Split(string(supscreen.Sanitize(raw)), "\n")
	for i, line := range lines {
		lines[i] = expandTabs(line)
	}
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// tabStop is the width of a tab in a terminal.
const tabStop = 8

// expandTabs turns each tab into the spaces that reach the next tab stop.
// ansi.Truncate and ansi.StringWidth count a tab as no width at all, while a
// terminal moves to the stop: a line with tabs would overflow the panel and
// push the borders of the grid. Colours (SGR) take no room and are kept.
func expandTabs(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var out strings.Builder
	col := 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b { // an SGR: ESC [ ... m, the only escape Sanitize leaves
			end := i + 1
			for end < len(line) && (end == i+1 || line[end] < 0x40 || line[end] > 0x7e) {
				end++
			}
			end = min(end+1, len(line))
			out.WriteString(line[i:end])
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		if r == '\t' {
			n := tabStop - col%tabStop
			out.WriteString(strings.Repeat(" ", n))
			col += n
		} else {
			out.WriteString(line[i : i+size])
			col += ansi.StringWidth(line[i : i+size])
		}
		i += size
	}
	return out.String()
}

// crop is the screen in width cells and height lines, without wrapping: a line
// that is too wide is cut (colours kept, and closed so they do not leak into
// what is drawn next), and of a screen that is too tall the bottom is shown, where
// the prompt of the agent is. The result is exactly height lines.
func crop(lines []string, width, height int) []string {
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	out := make([]string, 0, height)
	for _, line := range lines {
		out = append(out, margin+ansi.Truncate(line, width, "")+"\x1b[m")
	}
	for len(out) < height {
		out = append(out, "")
	}
	return out
}

// retryHint ends the reason the panel shows when trying again can change it:
// the panel does not retry by itself, it does when the selection changes or the
// toggle is pressed twice.
const retryHint = " · press v twice to retry"

// liveReason is the line the panel shows in place of a screen, for the error a
// stream ended with. A code the person can act on has words of its own.
func liveReason(err error) string {
	var coded *domain.Error
	if !errors.As(err, &coded) {
		if errors.Is(err, io.EOF) {
			return "The stream ended"
		}
		return "The live view stopped: " + printable(err.Error())
	}
	switch coded.Code {
	case domain.CodeRunNotAttachable:
		return "This run cannot be watched now: it is over or has no session"
	case domain.CodeNotImplemented:
		return "This worker is too old for the live view: update it"
	case domain.CodeWorkerOffline:
		return "The worker is offline, so there is no live view"
	case domain.CodePolicyDenied:
		return "Not allowed: " + printable(coded.Message)
	}
	return "The live view stopped: " + string(coded.Code) + " " + printable(coded.Message)
}

// retryable says whether a retry can change the reason: a Worker that is
// offline or a stream that broke may come back; a Run that is over, or a Worker
// too old for the live view, does not.
func retryable(err error) bool {
	var coded *domain.Error
	if !errors.As(err, &coded) {
		return true
	}
	switch coded.Code {
	case domain.CodeRunNotAttachable, domain.CodeNotImplemented, domain.CodePolicyDenied:
		return false
	}
	return true
}

// peerNames is who else is inside, by the names the Controller knows them by.
// A client that is inside more than once (two terminals of the same machine) is
// named once, with how many times: "mbp ×2".
func peerNames(peers []v1.AttachPeer) []string {
	var names []string
	count := map[string]int{}
	for _, p := range peers {
		name := printable(p.Name)
		if name == "" {
			name = shortRef(p.ID)
		}
		if count[name]++; count[name] == 1 {
			names = append(names, name)
		}
	}
	for i, name := range names {
		if n := count[name]; n > 1 {
			names[i] = name + " ×" + strconv.Itoa(n)
		}
	}
	return names
}

// alsoInside is the part of a title that says who is in the Run, at most three
// names: "inside: mbp, omarch01". It does not say "also": the Controller lists
// every client that is inside, and that can be this machine in another terminal.
func alsoInside(names []string) string {
	if len(names) == 0 {
		return ""
	}
	shown := names[:min(len(names), 3)]
	text := "inside: " + strings.Join(shown, ", ")
	if more := len(names) - len(shown); more > 0 {
		text += " +" + strconv.Itoa(more)
	}
	return text
}

// liveRun says whether a Run has a screen to watch: it is interactive and
// running on a Worker that can be reached. The Controller has the last word
// (attachable), and the panel shows what it says if it refuses.
func liveRun(r v1.Run) bool {
	return liveReasonOf(r) == ""
}

// liveReasonOf is why a Run has no live screen, or "" if it has.
func liveReasonOf(r v1.Run) string {
	switch {
	case r.Mode != domain.ModeInteractive:
		return "This run is headless: there is no screen to show"
	case r.State != domain.RunRunning && r.State != domain.RunWaitingInput:
		return "This run is not running"
	case r.Runtime != domain.RuntimeActive || r.WorkerID == "":
		return "This run has no session on a worker yet"
	case r.WorkerReachable != nil && !*r.WorkerReachable:
		return "The worker of this run is unreachable"
	}
	return ""
}
