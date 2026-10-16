package tmux

import (
	"testing"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
)

// Only the absence of a server means there are no sessions: any other failure
// leaves them unknown.
func TestNoServer(t *testing.T) {
	for stderr, want := range map[string]bool{
		"no server running on /tmp/tmux-501/cf-abc\n":                                 true,
		"error connecting to /tmp/tmux-501/cf-abc (No such file or directory)\n":      true,
		"error connecting to /tmp/tmux-501/cf-abc (Permission denied)\n":              false,
		"error connecting to /tmp/tmux-501/cf-abc (Socket operation on non-socket)\n": false,
		"directory /tmp/tmux-501 has unsafe permissions\n":                            false,
		"command list-sessions: unknown flag -Z\n":                                    false,
		"": false,
	} {
		if got := noServer(stderr); got != want {
			t.Errorf("%q: %t", stderr, got)
		}
	}
}

func TestWithoutAFilterTheCaptureIsAPlainCopy(t *testing.T) {
	if got := pipeCommand(cfruntime.Spec{Capture: "/r/output.log"}); got != "cat >> '/r/output.log'" {
		t.Errorf("pipe = %q", got)
	}
}

func TestAFilterIsQuotedAndAppendsToTheCapture(t *testing.T) {
	spec := cfruntime.Spec{Capture: "/r/it's.log", CaptureAck: "/r/ack", CaptureFilter: []string{"/bin/cf", "worker", "redact-pipe", "--run-dir", "/r/a b;c"}}
	want := `exec '/bin/cf' 'worker' 'redact-pipe' '--run-dir' '/r/a b;c' >> '/r/it'\''s.log'`
	if got := pipeCommand(spec); got != want {
		t.Errorf("pipe = %q, want %q", got, want)
	}
	if err := checkPaths(spec); err != nil {
		t.Error(err)
	}
}

func TestAFilterNeedsACaptureAndAnAcknowledgementAndNoHash(t *testing.T) {
	for _, spec := range []cfruntime.Spec{
		{Capture: "/r/out", CaptureFilter: []string{"/bin/cf"}},
		{CaptureAck: "/r/ack", CaptureFilter: []string{"/bin/cf"}},
		{Capture: "/r/out", CaptureAck: "/r/ack", CaptureFilter: []string{"/bin/cf", "#{pane_pid}"}},
		{Capture: "/r/out", CaptureAck: "/r/#ack", CaptureFilter: []string{"/bin/cf"}},
	} {
		if err := checkPaths(spec); err == nil {
			t.Errorf("checkPaths(%+v) = nil", spec)
		}
	}
}
