//go:build integration

package relay

import (
	"fmt"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// asCF makes the test binary behave as `cf relay ...`, which is what the hook
// of the repositories runs.
const asCF = "CF_TEST_AS_CF"

func TestMain(m *testing.M) {
	if os.Getenv(asCF) == "1" {
		root := &cobra.Command{Use: "cf", SilenceUsage: true}
		root.AddCommand(Command())
		root.SetArgs(os.Args[1:])
		if err := root.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
