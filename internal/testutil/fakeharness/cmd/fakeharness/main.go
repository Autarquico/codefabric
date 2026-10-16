// Command fakeharness is the program of package fakeharness on its own: a
// harness that does what its script says.
package main

import (
	"os"

	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

func main() {
	os.Exit(fakeharness.Main(os.Args, os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}
