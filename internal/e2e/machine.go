//go:build e2e && (linux || darwin)

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
)

// systemPath follows the bin of a box in its PATH: what a machine has
// without anything installed by its person.
const systemPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// linkTools links git and tmux, from the PATH of the test, into the bin of
// the box. Nothing else of that PATH reaches the box: a codex or a claude the
// developer installed would make its Worker offer a harness nobody logged in
// to, and a Run of it run the real one. A box has the harnesses Install puts
// in its bin, and no other.
func (b *Box) linkTools() {
	b.h.t.Helper()
	for _, tool := range []string{"git", "tmux"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			b.h.t.Fatalf("e2e: %s: %v", tool, err)
		}
		if err := os.Symlink(path, filepath.Join(b.dirs.bin, tool)); err != nil {
			b.h.t.Fatal(err)
		}
	}
}

// Install puts an executable named name, with script as its content, in the
// PATH of the box.
func (b *Box) Install(name, script string) {
	b.h.t.Helper()
	if err := os.WriteFile(filepath.Join(b.dirs.bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // an executable of the test
		b.h.t.Fatal(err)
	}
}

// WriteHome writes content at rel in the HOME of the box, making its directories.
func (b *Box) WriteHome(rel, content string) {
	b.h.t.Helper()
	path := filepath.Join(b.dirs.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		b.h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		b.h.t.Fatal(err)
	}
}

// ReadHome is the content at rel in the HOME of the box, or "<error>" if it
// cannot be read.
func (b *Box) ReadHome(rel string) string {
	raw, err := os.ReadFile(filepath.Join(b.dirs.home, filepath.FromSlash(rel))) //nolint:gosec // the box of the test
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(raw)
}

// fakeCodex answers `codex --version` and runs `codex exec --json` as Codex
// 0.160.1 would print one turn: the events on stdout. Before them it prints the
// configuration of the Run (CODEX_HOME) on stderr, so that a secret rendered
// into it reaches the output of the Run, and it leaves codex.txt in the
// worktree (-C).
const fakeCodex = `#!/bin/sh
case "$1" in --version) echo "codex-cli 0.160.1"; exit 0 ;; esac
[ "$1" = exec ] || exit 64
wt=. prev=
for a in "$@"; do [ "$prev" = -C ] && wt=$a; prev=$a; done
cat "$CODEX_HOME/config.toml" >&2
echo codex > "$wt/codex.txt"
echo '{"type":"thread.started","thread_id":"e2e-thread"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ok"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`

// fakeClaude only answers `claude --version`: a machine with Claude Code installed.
const fakeClaude = `#!/bin/sh
case "$1" in --version) echo "2.1.314 (Claude Code)"; exit 0 ;; esac
exit 64
`

// InstallHarnesses makes the box a machine with Codex and Claude Code installed
// and logged in, whatever the machine running the tests has: fake codex and
// claude in its own PATH, the login files of both in its HOME, and a
// `security` that finds the Keychain item of Claude Code on macOS. The doctor
// of its Worker finds both green.
func (b *Box) InstallHarnesses() {
	b.h.t.Helper()
	b.Install("codex", fakeCodex)
	b.Install("claude", fakeClaude)
	b.Install("security", "#!/bin/sh\nexit 0\n")
	b.WriteHome(".codex/auth.json", "{}")
	b.WriteHome(".claude/.credentials.json", "{}")
}
