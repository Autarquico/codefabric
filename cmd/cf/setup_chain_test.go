package main

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/support/prompt"
)

// chained runs `cf setup --role both` with the real runCommand and the real
// adopt step, over a root whose `workflow adopt` is a probe that records what
// the command it chains sees of its input.
func chained(t *testing.T, in io.Reader) (sawTerminal bool, adoptLine, joinAnswer string) {
	t.Helper()
	steps := binarySetupSteps()
	steps.connect = func(context.Context, *cobra.Command, *prompt.Prompter) error { return nil }
	steps.join = func(_ context.Context, _ *cobra.Command, p *prompt.Prompter) error {
		var err error
		joinAnswer, err = p.Ask("join", "")
		return err
	}
	root := &cobra.Command{Use: "cf", SilenceUsage: true, SilenceErrors: true}
	workflow := &cobra.Command{Use: "workflow"}
	workflow.AddCommand(&cobra.Command{
		Use: "adopt",
		RunE: func(cmd *cobra.Command, _ []string) error {
			terminal, ok := cmd.InOrStdin().(interface{ IsTerminal() bool })
			sawTerminal = ok && terminal.IsTerminal()
			shared, ok := cmd.InOrStdin().(interface{ Buffer() *bufio.Reader })
			if !ok {
				return nil
			}
			adoptLine, _ = shared.Buffer().ReadString('\n')
			return nil
		},
	})
	root.AddCommand(workflow, newSetupCmd(steps))
	root.SetIn(in)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"setup", "--role", "both"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return sawTerminal, adoptLine, joinAnswer
}

func TestSetup_AdoptKeepsTheTerminalAndTheSharedInput(t *testing.T) {
	script := "s\nmi-workflow\nrespuesta-de-adopt\nrespuesta-de-join\n"
	terminal := prompt.New(strings.NewReader(script), io.Discard, prompt.AsTerminal(true)).Input()
	saw, adoptLine, joinAnswer := chained(t, terminal)
	if !saw {
		t.Error("`workflow adopt` takes the input of `cf setup` for a pipe: it would demand --yes")
	}
	if adoptLine != "respuesta-de-adopt\n" || joinAnswer != "respuesta-de-join" {
		t.Errorf("the input was not shared: adopt read %q, join read %q", adoptLine, joinAnswer)
	}
}

func TestSetup_AdoptStaysHeadlessWithoutATerminal(t *testing.T) {
	saw, _, _ := chained(t, strings.NewReader("s\nmi-workflow\nx\ny\n"))
	if saw {
		t.Error("a pipe was passed to `workflow adopt` as a terminal")
	}
}
