//go:build linux || darwin

package gitops

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
