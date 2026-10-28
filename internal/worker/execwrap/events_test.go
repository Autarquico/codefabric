//go:build linux || darwin

package execwrap_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestStructuredStdoutGoesToEventsAndStderrToTheLog(t *testing.T) {
	script := `i=0; while [ $i -lt 200 ]; do echo "{\"n\":$i}"; echo "err $i" >&2; i=$((i+1)); done; printf '{"last":true}'`
	s := launch(t, execwrap.Launch{Argv: shell(script), Grace: time.Second, StructuredStdout: true}, true)
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v", exit)
	}

	events := readFile(t, s.dir, execwrap.EventsFile)
	lines := strings.Split(events, "\n")
	if len(lines) != 201 || lines[0] != `{"n":0}` || lines[199] != `{"n":199}` || lines[200] != `{"last":true}` {
		t.Errorf("events.jsonl has %d lines, the last %q: the last line has no newline and must be kept", len(lines), lines[len(lines)-1])
	}
	log := readFile(t, s.dir, execwrap.OutputLog)
	if strings.Count(log, "err ") != 200 || strings.Contains(log, `{"n"`) {
		t.Errorf("output.log must hold the stderr and no stdout: %.200q", log)
	}
	if pane := s.pane.String(); strings.Contains(pane, `{"n"`) || !strings.Contains(pane, "err 199") {
		t.Errorf("the pane must show the stderr only: %.200q", pane)
	}
}

func TestWithoutStructuredStdoutThereIsNoEventsFileAndBothStreamsGoToTheLog(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("echo out; echo err >&2"), Grace: time.Second}, true)
	s.waitExit()
	if _, err := os.Stat(filepath.Join(s.dir, execwrap.EventsFile)); !os.IsNotExist(err) {
		t.Errorf("events.jsonl exists without StructuredStdout: %v", err)
	}
	log := readFile(t, s.dir, execwrap.OutputLog)
	if !strings.Contains(log, "out") || !strings.Contains(log, "err") {
		t.Errorf("output.log = %q", log)
	}
}

func TestStructuredStdoutIsIgnoredInInteractiveMode(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("echo out"), Mode: execwrap.ModeInteractive, Grace: time.Second, StructuredStdout: true}, true)
	s.waitExit()
	if _, err := os.Stat(filepath.Join(s.dir, execwrap.EventsFile)); !os.IsNotExist(err) {
		t.Errorf("events.jsonl exists in interactive mode: %v", err)
	}
}

func TestEventsAreRedactedInEveryFormTheValueTakesInJSON(t *testing.T) {
	const value = `ab"cd\ef/gh` + "\n" + `second-line-9x`
	forms := []string{
		value, // raw, in a line that is not JSON
		`ab\"cd\\ef/gh\nsecond-line-9x`,
		`ab\"cd\\ef\/gh\nsecond-line-9x`,
		`ab\"cd\\ef/gh\u000asecond-line-9x`,
		`ab\"cd\\ef/gh\u000Asecond-line-9x`,
	}
	var script strings.Builder
	for i, f := range forms[1:] {
		script.WriteString(`printf '%s\n' '{"i":` + string(rune('0'+i)) + `,"v":"` + f + `"}'; `)
	}
	script.WriteString(`echo err-stays >&2; printf '{"v":"%s"}' 'ab\"cd\\ef/gh\nsecond-line-9x'`)

	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(value)})
	l := execwrap.Launch{
		Argv: shell(script.String()), SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: apiRef}},
		Grace: time.Second, StructuredStdout: true,
	}
	s := launchIn(t, dir, l, true, withDelivery+"=1")
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v", exit)
	}

	events := readFile(t, dir, execwrap.EventsFile)
	for _, f := range forms {
		if strings.Contains(events, f) || strings.Contains(events, strings.Split(f, "\n")[0]) {
			t.Errorf("events.jsonl holds the value as %q:\n%s", f, events)
		}
	}
	lines := strings.Split(events, "\n")
	if len(lines) != 5 {
		t.Fatalf("events.jsonl = %q", events)
	}
	for _, line := range lines {
		var v struct{ V string }
		if err := json.Unmarshal([]byte(line), &v); err != nil || v.V != marker {
			t.Errorf("line %q: err %v, v = %q: it must be valid JSON with the value replaced by %s", line, err, v.V, marker)
		}
	}
	if log := readFile(t, dir, execwrap.OutputLog); !strings.Contains(log, "err-stays") {
		t.Errorf("output.log = %q", log)
	}
	noValueIn(t, value, dir)
}

func TestEventsWithSecretsSurviveAMatchThatWouldBreakTheJSONOfALine(t *testing.T) {
	const value = `abcdefg"` // a match that takes the closing quote of the line
	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(value)})
	l := execwrap.Launch{
		Argv:      shell(`echo '{"k":"abcdefg"}'; echo 'plain abcdefg" text'; echo '{"ok":1}'`),
		SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: apiRef}}, Grace: time.Second, StructuredStdout: true,
	}
	s := launchIn(t, dir, l, true, withDelivery+"=1")
	s.waitExit()

	got := readFile(t, dir, execwrap.EventsFile)
	want := `{"cf_redacted_line":true}` + "\nplain " + marker + " text\n" + `{"ok":1}` + "\n"
	if got != want {
		t.Errorf("events.jsonl = %q, want %q", got, want)
	}
	if bytes.Contains([]byte(got), []byte(value)) {
		t.Error("the value is in events.jsonl")
	}
}
