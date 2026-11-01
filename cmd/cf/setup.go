package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/prompt"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// Roles `cf setup` can set up. The Controller is not one of them (D9): it is
// installed once, on purpose, with `cf controller init`.
const (
	roleClient     = "client"
	roleWorker     = "worker"
	roleBoth       = "both"
	roleController = "controller"
)

// setupSteps are the flows `cf setup` chains. They are the ones of the
// commands of the same name, which only the binary can reach together (ADR 0001).
type setupSteps struct {
	connect func(ctx context.Context, cmd *cobra.Command, p *prompt.Prompter) error
	adopt   func(ctx context.Context, cmd *cobra.Command, p *prompt.Prompter) error
	join    func(ctx context.Context, cmd *cobra.Command, p *prompt.Prompter) error
}

func newSetupCmd(steps setupSteps) *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up this machine as a client, a Worker or both, step by step",
		Long: "Ask what this machine is for and chain the commands that set it up: `cf connect` for a\n" +
			"client, `cf worker join` (the wizard) for a Worker, and both with an optional\n" +
			"`cf workflow adopt` in between. The Controller is not set up here: use `cf controller init`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := prompt.New(cmd.InOrStdin(), cmd.OutOrStdout())
			if role == "" {
				var err error
				if role, err = askRole(p); err != nil {
					return err
				}
			}
			return runSetup(cmd, p, steps, role)
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "what this machine is: client, worker or both (default: ask)")
	return cmd
}

func runSetup(cmd *cobra.Command, p *prompt.Prompter, steps setupSteps, role string) error {
	ctx := cmd.Context()
	switch role {
	case roleController:
		_, err := fmt.Fprintln(p.Out(), "El rol Controller queda fuera de `cf setup`: instálalo con `cf controller init`.")
		return err
	case roleClient:
		return steps.connect(ctx, cmd, p)
	case roleWorker:
		return steps.join(ctx, cmd, p)
	case roleBoth:
		if err := steps.connect(ctx, cmd, p); err != nil {
			return err
		}
		adopt, err := p.Confirm("¿Crear un Workflow con la configuración de este equipo (cf workflow adopt)?", false)
		if err != nil {
			return err
		}
		if adopt {
			if err := steps.adopt(ctx, cmd, p); err != nil {
				return err
			}
		}
		return steps.join(ctx, cmd, p)
	}
	return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("--role must be client, worker or both, got %q", role), nil)
}

func askRole(p *prompt.Prompter) (string, error) {
	for {
		answer, err := p.Ask("¿Para qué sirve este equipo? (client, worker, both)", roleBoth)
		if err != nil {
			return "", err
		}
		switch role := strings.ToLower(answer); role {
		case roleClient, roleWorker, roleBoth, roleController:
			return role, nil
		}
		_, _ = fmt.Fprintln(p.Out(), "Elige client, worker o both.")
	}
}

// binarySetupSteps are the flows of the real commands.
func binarySetupSteps() setupSteps {
	return setupSteps{
		connect: func(ctx context.Context, cmd *cobra.Command, p *prompt.Prompter) error {
			invitation, err := p.Ask("Invitación de cliente", "")
			if err != nil {
				return err
			}
			return runCommand(ctx, cmd, p, []string{"connect"}, invitation)
		},
		adopt: func(ctx context.Context, cmd *cobra.Command, p *prompt.Prompter) error {
			name, err := p.Ask("Nombre del Workflow", "")
			if err != nil {
				return err
			}
			return runCommand(ctx, cmd, p, []string{"workflow", "adopt"}, name)
		},
		join: func(ctx context.Context, cmd *cobra.Command, p *prompt.Prompter) error {
			controller, _ := cmd.Flags().GetString("controller")
			return workerdaemon.JoinWizard(ctx, controller, p, "")
		},
	}
}

// runCommand runs the command at path under the same terminal as the prompts,
// with its flags at their defaults. The name of the command is looked up from
// the root, so `cf setup` does what the typed command does.
func runCommand(ctx context.Context, from *cobra.Command, p *prompt.Prompter, path []string, arg string) error {
	sub, _, err := from.Root().Find(path)
	if err != nil || sub == nil || sub.RunE == nil {
		return fmt.Errorf("cf setup: the command %q is not available", strings.Join(path, " "))
	}
	if err := sub.ParseFlags(nil); err != nil {
		return err
	}
	sub.SetContext(ctx)
	sub.SetIn(p.Input())
	sub.SetOut(from.OutOrStdout())
	sub.SetErr(from.ErrOrStderr())
	return sub.RunE(sub, []string{arg})
}
