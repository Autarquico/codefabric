package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
)

// The status of a shell is the person's: Ctrl-] and a clean exit are 0, any other
// status is the shell's and never a CF_* error; only the relay's errors stay CF_*.
// The TUI action (Finish) and the command read it through the same function.
func TestShellEnd_TheStatusOfTheShellIsNotAnAttachFailure(t *testing.T) {
	denied := domain.NewError(domain.CodePolicyDenied, "no", map[string]any{"policy": "project.shell"})
	for name, tc := range map[string]struct {
		runErr     error
		wantStatus int
		wantErr    error
	}{
		"ctrl-] or exit 0":    {nil, 0, nil},
		"exit 3":              {ExitStatus(3), 3, nil},
		"killed by signal":    {ExitStatus(255), 255, nil},
		"policy of the relay": {denied, 0, denied},
	} {
		t.Run(name, func(t *testing.T) {
			status, err := shellEnd(tc.runErr)
			if status != tc.wantStatus || !errors.Is(err, tc.wantErr) {
				t.Errorf("shellEnd = %d, %v", status, err)
			}
			finish := (&AttachSession{shell: true}).Finish(context.Background(), tc.runErr)
			if !errors.Is(finish, tc.wantErr) {
				t.Errorf("TUI Finish = %v, want %v", finish, tc.wantErr)
			}
		})
	}
}

// cflk shell ends with the status of the shell, without a CF_* line; a relay error keeps its code.
func TestReport_AShellStatusIsTheExitCodeAndPrintsNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	if got := report(&out, &errOut, ShellStatus(3), false, false); got != 3 || out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", got, out.String(), errOut.String())
	}
	denied := domain.NewError(domain.CodePolicyDenied, "no", nil)
	if got := report(&out, &errOut, denied, false, false); got != domain.ExitCode(domain.CodePolicyDenied) {
		t.Errorf("a refusal exits %d", got)
	}
}
