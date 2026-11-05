package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type call struct {
	name  string
	args  []string
	stdin []byte
}

func recorder(calls *[]call) Exec {
	return func(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
		*calls = append(*calls, call{name, args, stdin})
		return nil, nil
	}
}

func found(string) (string, error)   { return "/bin/x", nil }
func missing(string) (string, error) { return "", errors.New("not found") }

func TestOsascriptPassesTextAsArgvNotScript(t *testing.T) {
	var calls []call
	native := DetectNative("darwin", found, recorder(&calls))
	body := `x" & (do shell script "touch /tmp/pwned") & "`
	if err := native.Show(context.Background(), "-title", body); err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if c.name != "osascript" {
		t.Fatal(c.name)
	}
	for i, a := range c.args {
		if i%2 == 0 && i < 6 && a != "-e" {
			t.Errorf("arg %d = %q, want -e", i, a)
		}
		if i < 6 && i%2 == 1 && strings.Contains(a, "pwned") {
			t.Error("the text is in the script")
		}
	}
	if got := c.args[6:]; !slices.Equal(got, []string{" -title", body}) {
		t.Errorf("argv tail %q", got)
	}
}

func TestNotifySendArgv(t *testing.T) {
	var calls []call
	native := DetectNative("linux", found, recorder(&calls))
	if err := native.Show(context.Background(), "-t", "-b"); err != nil {
		t.Fatal(err)
	}
	if c := calls[0]; c.name != "notify-send" || !slices.Equal(c.args, []string{"--app-name=CodeFabric", "--", "-t", "-b"}) {
		t.Errorf("%+v", c)
	}
}

func TestDetectNative(t *testing.T) {
	if DetectNative("linux", missing, nil) != nil || DetectNative("darwin", missing, nil) != nil || DetectNative("windows", found, nil) != nil {
		t.Error("an adapter was picked where there is none")
	}
}

func TestOSExecBoundsOutputAndPassesStdin(t *testing.T) {
	out, err := OSExec(context.Background(), "sh", []string{"-c", "cat; head -c 100000 /dev/zero"}, []byte("in"))
	if err != nil || len(out) != maxOutput || !strings.HasPrefix(string(out), "in") {
		t.Fatalf("%d bytes, %v", len(out), err)
	}
}

func TestOSExecEndsWithContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := OSExec(ctx, "sleep", []string{"5"}, nil); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("not stopped: %v after %v", err, time.Since(start))
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                                 "plain",
		"\x1b[1;31mbold\x1b[0m":                 "bold",
		"a\x1b]8;;http://x\x07link\x1b]8;;\x07": "alink",
		"a\r\n\tb\x00c":                         "a b c",
		"\x1b[":                                 "",
		"a\u202ebc\u200bd":                      "abcd",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(Sanitize(strings.Repeat("é", 1000))); len(got) != maxText {
		t.Errorf("len %d", len(got))
	}
}

func TestSeenForgetsOldestAndExpires(t *testing.T) {
	now := time.Now()
	s := newSeen(2, time.Minute)
	s.add("a", now)
	s.add("b", now)
	s.add("c", now)
	if s.has("a", now) || !s.has("c", now) || len(s.at) != 2 {
		t.Error("not bounded")
	}
	if s.has("c", now.Add(time.Minute)) {
		t.Error("did not expire")
	}
}

func TestOSExecEndsWithContextWhenAChildHoldsTheOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := OSExec(ctx, "sh", []string{"-c", "sleep 5 & wait"}, nil)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("not stopped: %v after %v", err, time.Since(start))
	}
}

func TestOSExecKillsAChildLeftHoldingTheOutput(t *testing.T) {
	start := time.Now()
	if _, err := OSExec(context.Background(), "sh", []string{"-c", "sleep 5 &"}, nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited %v for the backgrounded child", time.Since(start))
	}
}

func TestNotifySendEscapesMarkupInTheBody(t *testing.T) {
	args := notifySendArgs("t", `<a href="http://x">y</a> & z`)
	if got, want := args[len(args)-1], "&lt;a href=&#34;http://x&#34;&gt;y&lt;/a&gt; &amp; z"; got != want {
		t.Errorf("body %q, want %q", got, want)
	}
}
