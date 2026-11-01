package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/support/prompt"
	"github.com/Autarquico/codefabric/internal/worker/setup"
)

// setupRecorder are the steps `cf setup` ran, in order.
type setupRecorder struct {
	ran     []string
	failing string
}

func (r *setupRecorder) steps() setupSteps {
	step := func(name string) func(context.Context, *cobra.Command, *prompt.Prompter) error {
		return func(context.Context, *cobra.Command, *prompt.Prompter) error {
			r.ran = append(r.ran, name)
			if r.failing == name {
				return errors.New(name + " failed")
			}
			return nil
		}
	}
	return setupSteps{connect: step("connect"), adopt: step("adopt"), join: step("join")}
}

func runSetupCmd(t *testing.T, r *setupRecorder, stdin string, args ...string) (code int, out string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code = cli.RunContextWith(t.Context(), options(), append([]string{"setup"}, args...), strings.NewReader(stdin), &stdout, &stderr, newSetupCmd(r.steps()))
	return code, stdout.String() + stderr.String()
}

func TestSetup_RolesChainTheirSteps(t *testing.T) {
	for _, tc := range []struct {
		name, stdin string
		args        []string
		want        []string
	}{
		{"client", "", []string{"--role", "client"}, []string{"connect"}},
		{"worker", "", []string{"--role", "worker"}, []string{"join"}},
		{"both, adopting", "s\n", []string{"--role", "both"}, []string{"connect", "adopt", "join"}},
		{"both, not adopting", "\n", []string{"--role", "both"}, []string{"connect", "join"}},
		{"asked: worker", "worker\n", nil, []string{"join"}},
		{"asked: the default is both", "\n\n", nil, []string{"connect", "join"}},
		{"asked again after a wrong answer", "tal\nclient\n", nil, []string{"connect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r setupRecorder
			code, out := runSetupCmd(t, &r, tc.stdin, tc.args...)
			if code != 0 || !slices.Equal(r.ran, tc.want) {
				t.Errorf("exit %d, ran %v, want %v: %s", code, r.ran, tc.want, out)
			}
		})
	}
}

func TestSetup_ControllerIsOutOfScope(t *testing.T) {
	for _, tc := range []struct {
		stdin string
		args  []string
	}{{"", []string{"--role", "controller"}}, {"controller\n", nil}} {
		var r setupRecorder
		code, out := runSetupCmd(t, &r, tc.stdin, tc.args...)
		if code != 0 || len(r.ran) != 0 || !strings.Contains(out, "cf controller init") {
			t.Errorf("exit %d, ran %v: %s", code, r.ran, out)
		}
	}
}

func TestSetup_RejectsAnUnknownRoleAndStopsAtAFailedStep(t *testing.T) {
	var r setupRecorder
	if code, _ := runSetupCmd(t, &r, "", "--role", "admin"); code != 2 || len(r.ran) != 0 {
		t.Errorf("unknown role: exit %d, ran %v; want 2 and nothing", code, r.ran)
	}
	r = setupRecorder{failing: "connect"}
	if code, _ := runSetupCmd(t, &r, "n\n", "--role", "both"); code == 0 || !slices.Equal(r.ran, []string{"connect"}) {
		t.Errorf("a failed connect: exit %d, ran %v; want failure and nothing after it", code, r.ran)
	}
}

func TestSetup_ClientRoleConnects(t *testing.T) {
	_, clientInvite := bringUpController(t)
	box := machine(t)
	code, _, errOut := cf(t, clientInvite+"\n", "setup", "--role", "client")
	if code != 0 {
		t.Fatalf("setup --role client: exit %d: %s", code, errOut)
	}
	if exists, err := client.CredentialsExist(box.CredentialsDir()); err != nil || !exists {
		t.Errorf("client credentials: %v, %v", exists, err)
	}
}

func TestSetup_WorkerRoleRunsTheWizardAndEndsTheSetup(t *testing.T) {
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	// invitation, name, cpus, memory, availability, disk, then no harness offered.
	code, out, errOut := cf(t, invite+"\nbox\n\n\n\n1MiB\nn\nn\n", "setup", "--role", "worker")
	if code != 0 {
		t.Fatalf("setup --role worker: exit %d: %s\n%s", code, errOut, out)
	}
	if _, pending, err := setup.Load(box); err != nil || pending {
		t.Errorf("setup pending=%v err=%v\n%s", pending, err, out)
	}
	if !strings.Contains(out, "Listo: el Worker box") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWorkerJoin_WithoutTerminalIsAsBefore(t *testing.T) {
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	if code, _, _ := cf(t, "", "worker", "join"); code != 2 {
		t.Errorf("join without invitation and without terminal: exit %d, want 2", code)
	}
	if code, _, _ := cf(t, "", "worker", "join", "--resume", "--non-interactive"); code != 2 {
		t.Errorf("--resume --non-interactive: exit %d, want 2", code)
	}
	if code, out, errOut := cf(t, invite+"\n", "worker", "join", "-", "--name", "plain"); code != 0 || !strings.Contains(out, "Joined") {
		t.Fatalf("join: exit %d: %s %s", code, out, errOut)
	}
	// The classic join asks nothing and leaves nothing pending.
	if _, pending, err := setup.Load(box); err != nil || pending {
		t.Errorf("setup pending=%v err=%v", pending, err)
	}
}
