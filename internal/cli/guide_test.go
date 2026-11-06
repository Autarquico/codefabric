package cli

import (
	"os"
	"strings"
	"testing"
)

// The CLI reference is written by hand; this is what makes it fail when a
// command is added to cf and the guide is not.
func TestGuideCoversEveryCommand(t *testing.T) {
	doc, err := os.ReadFile("../../docs/guide/cli.md")
	if err != nil {
		t.Fatalf("the CLI guide is missing: %v", err)
	}
	headings := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		headings[strings.TrimSpace(line)] = true
	}
	for _, cmd := range newRootCmd(Options{}).Commands() {
		if cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" {
			continue
		}
		if want := "## `cf " + cmd.Name() + "`"; !headings[want] {
			t.Errorf("docs/guide/cli.md has no heading %q", want)
		}
	}
}
