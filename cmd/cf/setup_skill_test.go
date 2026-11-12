package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/support/prompt"
)

func TestSetup_OffersTheSkillToAClientOnlyOnATerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		want     bool
	}{{"terminal", true, true}, {"pipe", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			offered := false
			steps := setupSteps{
				connect: func(context.Context, *cobra.Command, *prompt.Prompter) error { return nil },
				skill:   func(*prompt.Prompter) error { offered = true; return nil },
			}
			p := prompt.New(strings.NewReader(""), io.Discard, prompt.AsTerminal(tc.terminal))
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			if err := runSetup(cmd, p, steps, roleClient); err != nil {
				t.Fatal(err)
			}
			if offered != tc.want {
				t.Errorf("offered = %v, want %v", offered, tc.want)
			}
		})
	}
}
