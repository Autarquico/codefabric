//go:build linux || darwin

package execwrap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/support/redact"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
)

// The answer of a Run is read from events.jsonl, which the wrapper redacts line by
// line, so a secret of the Run that a harness repeats in its answer is not in it.
// This covers the literal value; the forms it takes inside JSON are the concern
// of the events tests, and are not claimed here.
func TestTheLiteralValueOfASecretDoesNotReachTheAnswer(t *testing.T) {
	const value = "hunter2-literal-secret"
	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(value)})
	l := execwrap.Launch{
		Argv:      shell(`echo '{"type":"result","subtype":"success","result":"the key is ` + value + `, done"}'`),
		SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: apiRef}}, Grace: time.Second, StructuredStdout: true,
	}
	s := launchIn(t, dir, l, true, withDelivery+"=1")
	s.waitExit()

	path := filepath.Join(dir, execwrap.EventsFile)
	f, err := os.Open(path) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ch, err := claude.Adapter{}.Events(t.Context(), harness.Handle{EventsPath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	var answer string
	for ev := range ch {
		if ev.Kind == harness.EventResult {
			answer = ev.Result.Summary
		}
	}
	if strings.Contains(answer, value) || !strings.Contains(answer, marker) {
		t.Errorf("answer = %q", answer)
	}
}
