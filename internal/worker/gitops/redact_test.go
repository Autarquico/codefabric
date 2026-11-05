//go:build linux || darwin

package gitops

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

const cutValue = "ghp-cut-sentinel-0123456789abcdef0123456789abcdef0123456789abcdef"

// failingGit is a Git whose git prints stderr and fails.
func failingGit(t *testing.T, stderr string) *Git {
	t.Helper()
	dir := t.TempDir()
	text := filepath.Join(dir, "stderr")
	if err := os.WriteFile(text, []byte(stderr), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat '"+text+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	return &Git{git: bin, log: slog.New(slog.DiscardHandler)}
}

// A value git quotes where its stderr is cut, at the tail kept for the
// message or at the head kept of a chatty child, leaves no part of it.
func TestAValueAtACutOfTheStderrOfGitLeavesNoFragment(t *testing.T) {
	set, err := redact.NewSet([]redact.Needle{{Ref: "github.personal", Value: []byte(cutValue)}})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("y", 99) + "\n"
	head := strings.Repeat(line, (stderrBytes-40)/len(line))
	head += strings.Repeat("y", stderrBytes-40-len(head)) // the cut leaves 40 bytes of the value
	for name, stderr := range map[string]string{
		"the tail kept for the message": `error: open("leak-` + cutValue + strings.Repeat("z", 2008) + "\n",
		"the head kept of the stderr":   head + cutValue + "\nfatal: adding files failed\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := failingGit(t, stderr).ok(t.Context(), cmd{dir: t.TempDir(), timeout: time.Minute, args: []string{"add"}, values: set})
			if err == nil {
				t.Fatal("git did not fail")
			}
			for i := 0; i+redact.MinSecretLen <= len(cutValue); i++ {
				if strings.Contains(err.Error(), cutValue[i:i+redact.MinSecretLen]) {
					t.Fatalf("the message holds part of the value: %q", err.Error()[max(0, len(err.Error())-200):])
				}
			}
		})
	}
}

// scriptGit is a Git whose git is the shell script body.
func scriptGit(t *testing.T, body string) *Git {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	return &Git{git: bin, log: slog.New(slog.DiscardHandler)}
}

func cutSet(t *testing.T) *redact.Set {
	t.Helper()
	set, err := redact.NewSet([]redact.Needle{{Ref: "github.personal", Value: []byte(cutValue)}})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// noFragment fails if text holds any MinSecretLen bytes of the value.
func noFragment(t *testing.T, what, text string) {
	t.Helper()
	for i := 0; i+redact.MinSecretLen <= len(cutValue); i++ {
		if strings.Contains(text, cutValue[i:i+redact.MinSecretLen]) {
			t.Fatalf("%s holds part of the value: %q", what, text[max(0, len(text)-200):])
		}
	}
}

func TestTheStdoutOfGitIsRedactedToo(t *testing.T) {
	g := scriptGit(t, "echo 'remote: "+cutValue+"'")
	res, err := g.run(t.Context(), cmd{dir: t.TempDir(), timeout: time.Minute, args: []string{"fetch"}, values: cutSet(t)})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.stdout); got != "remote: [REDACTED:github.personal]" {
		t.Fatalf("stdout = %q", got)
	}
}

// The cap of the stdout cuts the value in the middle; the redaction came first.
func TestAValueAtTheCapOfTheStdoutOfGitLeavesNoFragment(t *testing.T) {
	g := scriptGit(t, "printf 'yyyyyyyy"+cutValue+"\\n'")
	for _, room := range []int{9, 20, 8 + len(cutValue)/2, 8 + len(cutValue) - 1} {
		res, err := g.run(t.Context(), cmd{dir: t.TempDir(), timeout: time.Minute, args: []string{"fetch"}, values: cutSet(t), maxOut: room})
		if err != nil {
			t.Fatal(err)
		}
		noFragment(t, "stdout", res.stdout)
	}
}

// Whatever the end of the child, the start of a value the redaction holds
// back is written as a marker and never as the fragment.
func TestWhatTheRedactionHoldsBackIsFlushedOnEveryEnd(t *testing.T) {
	half := cutValue[:len(cutValue)/2]
	// The cancelled child tells through a fifo that it has printed; only then is it cancelled.
	// It then blocks opening a fifo nobody writes, so only the cancellation ends it.
	dir := t.TempDir()
	printed, never := filepath.Join(dir, "printed"), filepath.Join(dir, "never")
	for _, fifo := range []string{printed, never} {
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		body    string
		cancel  bool
		wantErr bool
		exit    int
	}{
		"success":      {body: "printf '" + half + "'; printf '" + half + "' >&2"},
		"non-zero":     {body: "printf '" + half + "'; printf '" + half + "' >&2; exit 3", exit: 3},
		"cancellation": {body: "printf '" + half + "'; printf '" + half + "' >&2; echo > '" + printed + "'; read _ < '" + never + "'", cancel: true, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				go func() {
					defer cancel()
					if f, err := os.Open(printed); err == nil { //nolint:gosec // the fifo of the test; blocks until the child has printed
						_, _ = f.Read(make([]byte, 1))
						_ = f.Close()
					}
				}()
			}
			res, err := scriptGit(t, tc.body).run(ctx, cmd{dir: t.TempDir(), timeout: time.Minute, args: []string{"fetch"}, values: cutSet(t)})
			if (err != nil) != tc.wantErr || res.exit != tc.exit {
				t.Fatalf("err = %v, exit = %d", err, res.exit)
			}
			noFragment(t, "stdout", res.stdout)
			noFragment(t, "stderr", res.stderr)
			if !strings.Contains(res.stdout, redact.PartialMarker) || !strings.Contains(res.stderr, redact.PartialMarker) {
				t.Fatalf("the held-back start of the value was lost: stdout %q, stderr %q", res.stdout, res.stderr)
			}
		})
	}
}

func TestAGitThatCannotStartLeavesNothingHeld(t *testing.T) {
	g := &Git{git: filepath.Join(t.TempDir(), "no-such-git"), log: slog.New(slog.DiscardHandler)}
	res, err := g.run(t.Context(), cmd{dir: t.TempDir(), timeout: time.Minute, args: []string{"fetch"}, values: cutSet(t)})
	if err == nil || res.stdout != "" || res.stderr != "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
