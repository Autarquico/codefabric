//go:build linux || darwin

package execwrap_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// fakeCodex prints the recording of `codex exec --json` 0.160.1 to stdout, a
// line at a time, with notes on stderr in between, one of them shaped like the
// usage of a turn: it must reach the log and never the events.
const fakeCodex = `#!/bin/sh
[ -f "$CODEX_HOME/config.toml" ] || exit 64
[ "$1" = exec ] && case " $* " in *" --json "*) ;; *) exit 66 ;; esac
echo "Reading additional input from stdin..." >&2
while IFS= read -r line; do
  printf '%s\n' "$line"
  echo '{"type":"turn.completed","usage":{"input_tokens":999999,"output_tokens":999999}}' >&2
done < 'RECORDING'
`

// TestARunOfCodexKeepsItsEventsApartFromItsStderr runs the headless Launch of
// the codex adapter, with a fake binary, through the real wrapper: what the
// adapter reads from events.jsonl is the stdout of codex and nothing else.
func TestARunOfCodexKeepsItsEventsApartFromItsStderr(t *testing.T) {
	recording, err := filepath.Abs(filepath.Join("..", "..", "adapters", "harness", "codex", "testdata", "exec-0.160.1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(recording) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(strings.Replace(fakeCodex, "RECORDING", recording, 1)), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, "config.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := harness.RunSpec{
		RunID: "run-1", LeaseEpoch: 1, Mode: domain.ModeHeadless, Prompt: "say ok", RunDir: dir, ConfigDir: config,
		Worktree: t.TempDir(), OutputPath: filepath.Join(dir, "output.log"), EventsPath: filepath.Join(dir, "events.jsonl"),
	}
	host, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: []string{"PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	l, err := codex.Adapter{}.Start(t.Context(), host, run)
	if err != nil {
		t.Fatal(err)
	}
	if !l.StructuredStdout {
		t.Fatal("the headless Launch of codex does not ask for the structured stdout")
	}

	s := launchIn(t, dir, executionOf(l, bin, run.Worktree), true)
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v; the pane said %q", exit, s.pane.String())
	}

	events := readFile(t, dir, "events.jsonl")
	if events != string(want) {
		t.Errorf("events.jsonl is not the stdout of codex:\n%s\nwant\n%s", events, want)
	}
	log := readFile(t, dir, "output.log")
	if !strings.Contains(log, "Reading additional input from stdin...") || strings.Contains(log, `"type":"thread.started"`) {
		t.Errorf("output.log must hold the stderr and no stdout: %q", log)
	}
	if strings.Contains(events, "999999") || strings.Contains(events, "Reading additional") {
		t.Errorf("stderr reached events.jsonl: %q", events)
	}

	h := harness.Handle{RunID: "run-1", LeaseEpoch: 1, Mode: domain.ModeHeadless, RunDir: dir, OutputPath: run.OutputPath, EventsPath: run.EventsPath}
	f, err := os.Open(h.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ch, err := codex.Adapter{}.Events(t.Context(), h, f)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []harness.EventKind
	for ev := range ch {
		kinds = append(kinds, ev.Kind)
	}
	if wantKinds := []harness.EventKind{harness.EventSessionStarted, harness.EventUsage, harness.EventResult}; !slices.Equal(kinds, wantKinds) {
		t.Errorf("events = %v, want %v", kinds, wantKinds)
	}
	usage, err := codex.Adapter{}.CollectUsage(t.Context(), nil, h)
	if err != nil || len(usage) != 1 {
		t.Fatalf("CollectUsage = %+v, %v; want the one usage of the turn", usage, err)
	}
	u := usage[0]
	if *u.InputTokens != 10326 || *u.CacheReadTokens != 7168 || *u.CacheWriteTokens != 0 || *u.OutputTokens != 5 || u.RequestCount != nil {
		t.Errorf("usage = %+v", u)
	}
}

// executionOf is what the executor writes to launch.json for l, with the
// binary argv[0] names found at bin.
func executionOf(l harness.Launch, bin, dir string) execwrap.Launch {
	argv := slices.Clone(l.Argv)
	argv[0] = bin
	return execwrap.Launch{Argv: argv, Env: l.Env, Dir: dir, Grace: time.Second, StructuredStdout: l.StructuredStdout}
}
