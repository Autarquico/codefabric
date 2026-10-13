// Command cf is the CodeFabric CLI, TUI and daemon binary.
package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

func main() { os.Exit(run()) }

// run composes the binary: the CLI with the terminal that attaches over ssh, and
// the commands of the daemons. Tests that need the binary to behave as cf call it.
func run() int { return cli.ExecuteWith(options(), commands()...) }

// options are what the CLI cannot reach itself (ADR 0001).
func options() cli.Options { return cli.Options{Terminal: sshTerminal{}} }

func commands() []*cobra.Command {
	return []*cobra.Command{daemon.Command(), workerdaemon.Command(), relay.Command()}
}
