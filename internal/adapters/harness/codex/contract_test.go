package codex_test

import (
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/harnesstest"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
)

// refToken is a marker the user wrote in config.toml themselves.
const refToken = "team/codex-token"

// fakeCodex imitates codex-cli 0.160.1 as far as an adapter can tell: its
// version, `exec --json` printing a recording (and a note on stderr, as the
// real one does while it reads stdin), and the interactive mode as a banner.
// It fails if the Run was not given a CODEX_HOME with a configuration and an
// auth, or an `exec` without --json. A prompt of "hold" never ends.
const fakeCodex = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.160.1"; exit 0; fi
[ -f "$CODEX_HOME/config.toml" ] || { echo "codex: CODEX_HOME has no config.toml" >&2; exit 64; }
[ -f "$CODEX_HOME/auth.json" ] || { echo "codex: CODEX_HOME has no auth.json" >&2; exit 65; }
for last; do :; done
if [ "$1" = "exec" ]; then
  case " $* " in *" --json "*) ;; *) echo "codex exec: no --json" >&2; exit 66 ;; esac
  echo "Reading additional input from stdin..." >&2
  if [ "$last" = "hold" ]; then
    echo '{"type":"thread.started","thread_id":"held"}'
    while :; do sleep 0.05; done
  fi
  exec cat '%s'
fi
echo "codex interactive: $last"
if [ "$last" = "hold" ]; then while :; do sleep 0.05; done; fi
`

func TestScenario_CodexAdapter_Contrato(t *testing.T) {
	recording, err := filepath.Abs(filepath.Join("testdata", "exec-0.160.1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	harnesstest.Run(t, harnesstest.Fixture{
		New: func(string) harness.Adapter { return codex.Adapter{} },
		Install: func(t *testing.T, binDir string) {
			t.Helper()
			script := strings.Replace(fakeCodex, "%s", recording, 1)
			if err := os.WriteFile(filepath.Join(binDir, codex.Binary), []byte(script), 0o700); err != nil { //nolint:gosec // an executable of the test
				t.Fatal(err)
			}
			for _, name := range []string{"cat", "sleep"} { // the PATH of a check has nothing else
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
					t.Fatal(err)
				}
			}
		},
		SeedHome: func(t *testing.T, home string) {
			t.Helper()
			copyTree(t, filepath.Join("testdata", "home"), home)
			cfg := filepath.Join(home, ".codex", "config.toml")
			b, err := os.ReadFile(cfg) //nolint:gosec // the home of the check
			if err != nil {
				t.Fatal(err)
			}
			marker := "\n[mcp_servers.linked.env]\nLINKED_TOKEN = \"" + harness.SecretMarker(refToken) + "\"\n"
			if err := os.WriteFile(cfg, append(b, marker...), 0o600); err != nil { //nolint:gosec // the home of the check
				t.Fatal(err)
			}
		},
		Secrets:         plainSecrets,
		CredentialFiles: []string{filepath.Join(".codex", "auth.json")},
		Mutate: func(t *testing.T, home string) string {
			t.Helper()
			if err := os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("Always answer in French.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return codex.AgentsComponent
		},
		PrepareRun: func(_ *testing.T, run *harness.RunSpec, hold bool) {
			run.Prompt = "smoke"
			if hold {
				run.Prompt = "hold"
			}
		},
		ExpectExit:   0,
		ExpectOutput: `"type":"turn.completed"`,
		EventsFile:   "events.jsonl",
		Recorded: func(t *testing.T, h harness.Handle) {
			t.Helper()
			events, err := os.ReadFile(filepath.Join("testdata", "exec-0.160.1.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(h.EventsPath, events, 0o600); err != nil { //nolint:gosec // the RunDir of the check
				t.Fatal(err)
			}
			if err := os.WriteFile(h.OutputPath, []byte("Reading additional input from stdin...\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		ExpectEvents: []harness.Event{
			{Kind: harness.EventSessionStarted, SessionID: "01a1819d-734d-7a60-88f8-293969dcf0bd"},
			{Kind: harness.EventUsage, Usage: &harness.UsageEvent{
				InputTokens: i64(10326), CacheReadTokens: i64(7168), CacheWriteTokens: i64(0), OutputTokens: i64(5),
			}},
			{Kind: harness.EventResult, Result: &harness.Result{Summary: "ok"}},
		},
		ExpectUsage: []harness.UsageEvent{{
			InputTokens: i64(10326), CacheReadTokens: i64(7168), CacheWriteTokens: i64(0), OutputTokens: i64(5),
		}},
	})
}

func i64(v int64) *int64 { return &v }

// copyTree copies the files under src to dst, mode included.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p) //nolint:gosec // testdata
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm()) //nolint:gosec // the home of a check
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Reproduces a bug the contract found (harnesstest checkLaunch): Start names
// the binary "codex" and leaves the lookup to whoever execs the Launch, with
// its PATH and not the host's. In the contract check of the smoke Run that ran
// the codex installed on the machine of the test instead of the fake.
func TestStartNamesTheBinaryByItsPath(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, codex.Binary), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	host, err := localhost.New(localhost.Options{Home: t.TempDir(), Path: binDir})
	if err != nil {
		t.Fatal(err)
	}
	l, err := codex.Adapter{}.Start(t.Context(), host, spec(domain.ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(binDir, codex.Binary); l.Argv[0] != want {
		t.Errorf("argv[0] = %q, want %q", l.Argv[0], want)
	}
}

// Reproduces a bug the contract found, intermittently (harnesstest
// checkEventsCancel, "blocked read", which cancels right after Events returns):
// Events closes its channel only when its read returns, so a Run that is
// quiet keeps it open after ctx ends until the source is closed.
func TestEventsClosesWhenItsContextEndsWithAReadBlocked(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := codex.Adapter{}.Events(ctx, harness.Handle{}, pr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = pw.Write([]byte(`{"type":"thread.started","thread_id":"t"}` + "\n")) }()
	if ev := <-ch; ev.Kind != harness.EventSessionStarted {
		t.Fatalf("event = %+v", ev)
	}
	cancel() // the goroutine is now blocked reading the next line
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an event after ctx ended")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel is still open a second after ctx ended")
	}
	_ = pr.Close()
}
