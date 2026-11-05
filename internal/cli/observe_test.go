package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestPrintLogKeepsEveryFieldOnOneLineAndFreeOfEscapes(t *testing.T) {
	l := v1.Log{
		Sequence: 7, Level: "in\x1b[2Jfo", Source: "worker:\x1b]0;x\x07w", RunID: "run\r1", Message: "a\nb\tc\x1b[31m!",
		Attrs: []byte(`{"k":"\u001b"}`),
	}
	var safe, raw bytes.Buffer
	printLog(&safe, l, false)
	printLog(&raw, l, true)
	if got := safe.String(); strings.ContainsAny(got, "\x1b\x07\r") || strings.Count(got, "\n") != 1 ||
		!strings.Contains(got, "worker:w/run1  a b c!  {") {
		t.Errorf("sanitised: %q", got)
	}
	if got := raw.String(); !strings.Contains(got, "\x1b[2J") {
		t.Errorf("raw: %q", got)
	}
}

func TestLogsFlagsAreCheckedBeforeAnythingIsAsked(t *testing.T) {
	isolatedMachine(t)
	for _, args := range [][]string{
		{"logs", "--run", "run-1", "--tail", "0"},
		{"logs", "--tail", "0"},
		{"logs", "--since", "yesterday"},
		{"logs", "--after", "5", "--tail", "3"},
		{"logs", "--limit", "5", "--tail", "3"},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
}

func TestObservationCommandsNeedCredentialsAndTheirArguments(t *testing.T) {
	isolatedMachine(t)
	for _, args := range [][]string{{"ps"}, {"events"}, {"events", "--follow"}, {"logs", "--run", "run-1"}, {"logs"}, {"logs", "--worker", "box", "--level", "error"}, {"logs", "--run", "run-1", "--structured"}, {"attach", "auth-race"}} {
		code, out, _ := run(t, append(args, "--json")...)
		if env := errorOf(t, out); code != 6 || env["code"] != string(domain.CodeUnauthenticated) {
			t.Errorf("%v: exit %d, %v", args, code, env)
		}
	}
	for _, args := range [][]string{{"attach"}, {"attach", "a", "b"}, {"ps", "extra"}, {"events", "extra"}, {"events", "--since", "yesterday"}} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
}

func TestLastLines(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\nc\n", 10, "a\nb\nc\n"},
		{"a\nb\nc\n", 1, "c\n"},
		{"", 3, ""},
		{"only", 1, "only"},
		{"\n\n\n", 2, "\n\n"},
	} {
		if got := string(lastLines([]byte(tc.in), tc.n)); got != tc.want {
			t.Errorf("lastLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	if countLines([]byte("a\nb")) != 2 || countLines([]byte("a\nb\n")) != 2 || countLines(nil) != 0 {
		t.Error("countLines")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if got, err := parseSince("2h", now); err != nil || !got.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("2h = %v, %v", got, err)
	}
	if got, err := parseSince("2026-10-01T00:00:00Z", now); err != nil || got.Day() != 1 {
		t.Errorf("RFC 3339 = %v, %v", got, err)
	}
	for _, bad := range []string{"yesterday", "-2h", "0s", ""} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
