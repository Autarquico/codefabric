// Command cf is the CodeFabric CLI, TUI and daemon binary.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client/workflowadopt"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

func main() { os.Exit(run()) }

// run composes the binary: the CLI with the terminal that attaches over ssh, and
// the commands of the daemons. Tests that need the binary to behave as cf call it.
func run() int { return cli.ExecuteWith(options(), commands()...) }

// options are what the CLI cannot reach itself (ADR 0001).
func options() cli.Options {
	return cli.Options{Terminal: sshTerminal{}, Workflows: workflowAdopter}
}

// workflowAdopter composes the adopt of `cf workflow`: the harness adapters of
// this binary over the configuration of this machine. A machine without a home
// directory fails when the adopter is used, not when cf starts.
func workflowAdopter(store workflowadopt.Store, confirmer workflowadopt.Confirmer) cli.WorkflowAdopter {
	registry, err := harness.NewRegistry(codex.Adapter{}, claude.Adapter{})
	if err != nil {
		return failedAdopter{err}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return failedAdopter{err}
	}
	host, err := localhost.New(localhost.Options{Home: home, Env: os.Environ()})
	if err != nil {
		return failedAdopter{err}
	}
	return localAdopter{
		Adopter:  &workflowadopt.Adopter{Registry: registry, Host: host, Store: store, Confirmer: confirmer},
		registry: registry, host: host,
	}
}

// localAdopter is the Adopter with what the checklist asks of the harnesses.
type localAdopter struct {
	*workflowadopt.Adopter
	registry *harness.Registry
	host     harness.Host
}

func (a localAdopter) Harnesses() []string { return a.registry.IDs() }

func (a localAdopter) Components(ctx context.Context, id string) ([]cli.WorkflowComponent, error) {
	ad, ok := a.registry.Get(id)
	if !ok {
		return nil, fmt.Errorf("no adapter for harness %q", id)
	}
	snap, err := ad.SnapshotConfig(ctx, a.host, harness.SnapshotOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the %s configuration: %w", id, err)
	}
	out := make([]cli.WorkflowComponent, len(snap.Components))
	for i, c := range snap.Components {
		out[i] = cli.WorkflowComponent{Name: c.Name, Kind: string(c.Kind)}
	}
	return out, nil
}

// failedAdopter reports why the machine cannot be adopted.
type failedAdopter struct{ err error }

func (failedAdopter) Harnesses() []string { return nil }
func (f failedAdopter) Components(context.Context, string) ([]cli.WorkflowComponent, error) {
	return nil, f.err
}

func (f failedAdopter) Prepare(context.Context, []workflowadopt.Selection) (*workflowadopt.Prepared, error) {
	return nil, f.err
}

func (f failedAdopter) Commit(context.Context, string, *workflowadopt.Prepared, int, string) (v1.WorkflowVersion, error) {
	return v1.WorkflowVersion{}, f.err
}

func commands() []*cobra.Command {
	return []*cobra.Command{daemon.Command(), workerdaemon.Command(), relay.Command()}
}
