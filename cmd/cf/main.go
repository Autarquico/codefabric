// Command cf is the CodeFabric CLI, TUI and daemon binary.
package main

import (
	"os"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

func main() {
	os.Exit(cli.Execute(daemon.Command(), workerdaemon.Command(), relay.Command()))
}
