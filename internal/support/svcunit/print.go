package svcunit

import (
	"encoding/json"
	"fmt"
	"io"
)

// PrintResult tells a person what Install or Uninstall did: the warnings to
// errOut, then, to out, the changes under changed or, if there were none,
// unchanged.
func PrintResult(out, errOut io.Writer, res Result, changed, unchanged string) error {
	printWarnings(errOut, res.Warnings)
	if !res.Changed {
		_, err := fmt.Fprintf(out, "%s (%s, %s).\n", unchanged, res.Unit, res.Path)
		return err
	}
	fmt.Fprintf(out, "%s (%s, %s):\n", changed, res.Unit, res.Path)
	for _, a := range res.Actions {
		fmt.Fprintf(out, "  %s\n", a)
	}
	return nil
}

// PrintFile shows a rendered file and where it goes, as JSON or for a person.
func PrintFile(out, errOut io.Writer, asJSON bool, path string, content []byte, warnings []string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(struct {
			Path     string   `json:"path"`
			Content  string   `json:"content"`
			Warnings []string `json:"warnings"`
		}{path, string(content), warnings})
	}
	printWarnings(errOut, warnings)
	_, err := fmt.Fprintf(out, "# %s\n%s", path, content)
	return err
}

// printWarnings writes each warning on a line of its own.
func printWarnings(errOut io.Writer, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}
}
