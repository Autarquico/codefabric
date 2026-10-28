//go:build linux || darwin

package execwrap_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// A value with a quote, a backslash, a slash, a newline, a tab and two control
// characters, as a harness may print it: as it is, as the TOML basic string of
// the configuration file it was rendered in, and as JSON.
const (
	specialValue = "pass\"word\\back/x\nta\tb-end\x01\x7f-1234"
	specialTOML  = `pass\"word\\back/x\nta\tb-end\u0001\u007f-1234`
	specialJSON  = `pass\"word\\back\/x\nta\tb-end\u0001\u007f-1234`
)

// eventLine is form as a line of the structured stream: inside a JSON object
// when the form is escaped, bare when it is the raw value.
func eventLine(form string) string {
	if form == specialValue {
		return form
	}
	return `{"v":"` + form + `"}`
}

func specialForms() []string { return []string{specialValue, specialTOML, specialJSON} }

func TestTheEscapedFormsOfAValueAreRedactedFromTheLogAndTheEvents(t *testing.T) {
	var script strings.Builder
	for _, f := range specialForms() {
		script.WriteString(`printf '%s\n' 'out: ` + f + `' >&2; `)
		script.WriteString(`printf '%s\n' '` + eventLine(f) + `'; `)
	}
	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(specialValue)})
	l := execwrap.Launch{
		Argv: shell(script.String()), RedactOutput: true, Grace: time.Second, StructuredStdout: true,
	}
	s := launchIn(t, dir, l, true, withDelivery+"=1")
	if exit := s.waitExit(); exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v", exit)
	}
	for name, out := range map[string]string{
		"output.log":   readFile(t, dir, execwrap.OutputLog),
		"pane":         s.pane.String(),
		"events.jsonl": readFile(t, dir, execwrap.EventsFile),
	} {
		for _, leak := range []string{"pass\"word", `pass\"word`, `back\/x`, `ta\tb`, "ta\tb"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s holds %q: %q", name, leak, out)
			}
		}
		if name != "events.jsonl" && strings.Count(out, marker) != 3 {
			t.Errorf("%s is %q: want the value redacted 3 times", name, out)
		}
	}
}

func TestTheEscapedFormsOfAValueAreRedactedFromTheInteractiveFilter(t *testing.T) {
	var pieces []string
	for _, f := range specialForms() {
		pieces = append(pieces, "out: "+strings.ReplaceAll(f, "\n", "\r\n")+"\r\n")
	}
	var out bytes.Buffer
	err := execwrap.RedactPipe(execwrap.PipeOptions{
		RunDir: t.TempDir(), In: chunked(pieces...), Out: &out,
		Retention: retained(redact.Needle{Ref: apiRef, Value: []byte(specialValue)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), strings.Repeat("out: "+marker+"\r\n", 3); got != want {
		t.Errorf("the log is %q, want %q", got, want)
	}
}
