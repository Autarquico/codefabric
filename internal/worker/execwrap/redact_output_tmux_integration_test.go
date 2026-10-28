//go:build integration && (linux || darwin)

package execwrap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// The secrets of a configuration file (codex's config.toml) are in files, not
// in the environment: a Run with RedactOutput and no SecretEnv gets its output
// redacted by the filter of the pane all the same, whether the harness prints
// the value in two goes or as the TOML string it was rendered in.
func TestRedactOutputRedactsUnderTmuxWithoutAnySecretEnv(t *testing.T) {
	for _, mode := range []string{execwrap.ModeHeadless, execwrap.ModeInteractive} {
		t.Run(mode, func(t *testing.T) {
			r := newTmux(t)
			dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(specialValue)})
			spec := session("run_redact_output", dir, mode, "1")
			prints := `printf '%s' '` + specialValue[:9] + `'; sleep 0.3; printf '%s\n' '` + specialValue[9:] + `'; ` +
				`printf 'token = "%s"\n' '` + specialTOML + `'`
			l := execwrap.Launch{Argv: shell(prints), Mode: mode, Grace: time.Second, RedactOutput: true, Dir: dir, Gate: spec.Ready}
			if err := execwrap.Write(dir, execwrap.LaunchFile, l); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			exit := waitExitIn(t, dir, 30*time.Second)
			if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
				t.Fatalf("exit.json = %+v", exit)
			}
			var log []byte
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				// The filter of an interactive Run may still be draining the terminal.
				if log, _ = os.ReadFile(filepath.Join(dir, execwrap.OutputLog)); strings.Count(string(log), marker) >= 2 { //nolint:gosec // a file of the test
					break
				}
			}
			if got := strings.Count(string(log), marker); got != 2 {
				t.Errorf("the log has the value redacted %d times, want 2: %q", got, log)
			}
			for _, leak := range []string{"pass\"word", `pass\"word`, "back/x", "1234"} {
				if strings.Contains(string(log), leak) {
					t.Errorf("the log has %q: %q", leak, log)
				}
			}
			noValueIn(t, specialValue, dir)
		})
	}
}
