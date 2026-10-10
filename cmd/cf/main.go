// Command cf is the CodeFabric CLI, TUI and daemon binary.
package main

import (
	"os"

	"github.com/Autarquico/codefabric/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
