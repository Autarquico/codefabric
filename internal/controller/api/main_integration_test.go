//go:build integration

package api_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/controller/relay"
)

// asCF makes the test binary behave as `cflk relay ...`, which is what the hook
// of the relay's repositories runs.
const asCF = "CF_TEST_AS_CF"

func TestMain(m *testing.M) {
	if os.Getenv(asCF) == "1" {
		root := &cobra.Command{Use: "cflk", SilenceUsage: true}
		root.AddCommand(relay.Command())
		root.SetArgs(os.Args[1:])
		if err := root.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
