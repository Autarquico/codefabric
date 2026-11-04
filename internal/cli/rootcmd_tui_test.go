package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
)

// terminalStream is a stream that says whether it is a terminal.
type terminalStream struct {
	bytes.Buffer
	tty bool
}

func (s *terminalStream) IsTerminal() bool { return s.tty }

// bareCf runs cf with the given arguments and reports how many times the TUI
// was opened.
func bareCf(t *testing.T, opts Options, args []string, inTTY, outTTY bool) (opened, code int, stdout string) {
	t.Helper()
	opts.openTUIClient = func(*cobra.Command) (*client.Client, error) { return &client.Client{}, nil }
	if opts.TUI != nil {
		launch := opts.TUI
		opts.TUI = func(ctx context.Context, c *client.Client) error { opened++; return launch(ctx, c) }
	}
	out := &terminalStream{tty: outTTY}
	var errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, args, &terminalStream{tty: inTTY}, out, &errOut)
	return opened, code, out.String()
}

func TestScenario_TuiShellHome_BareCfOpensTheTUIOnlyOnATerminal(t *testing.T) {
	launcher := Options{TUI: func(context.Context, *client.Client) error { return nil }}
	cases := []struct {
		name         string
		args         []string
		inTTY        bool
		outTTY       bool
		wantOpened   int
		wantCode     int
		wantHelpText bool
	}{
		{"terminal on both ends", nil, true, true, 1, 0, false},
		{"stdin piped", nil, false, true, 0, 0, true},
		{"stdout piped", nil, true, false, 0, 0, true},
		// As before the TUI: a group with --json is a usage error, not help.
		{"json asked for", []string{"--json"}, true, true, 0, 2, false},
		{"an unknown command is not the TUI", []string{"nope"}, true, true, 0, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opened, code, stdout := bareCf(t, launcher, c.args, c.inTTY, c.outTTY)
			if opened != c.wantOpened || code != c.wantCode {
				t.Fatalf("TUI opened %d times (want %d), exit %d (want %d)", opened, c.wantOpened, code, c.wantCode)
			}
			if got := strings.Contains(stdout, "Usage:"); got != c.wantHelpText {
				t.Errorf("help printed = %v, want %v: %q", got, c.wantHelpText, stdout)
			}
		})
	}
}

func TestScenario_TuiShellHome_WithoutALauncherBareCfPrintsHelp(t *testing.T) {
	_, _, stdout := bareCf(t, Options{}, nil, true, true)
	if !strings.Contains(stdout, "Usage:") {
		t.Errorf("no help printed: %q", stdout)
	}
}
