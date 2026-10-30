package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func newProjectExplainCmd() *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "explain <project>",
		Short: "Show the effective configuration of a Project at a commit, and where each field comes from",
		Long: "Show what a Run of the Project would be configured with at --ref (default: the default branch of the Project),\n" +
			"read from the clone in this directory, and the layer each field comes from: global, project, repo or clamped.\n" +
			"A .codefabric/project.yaml that is not valid is an error.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			e, err := c.ExplainProjectAt(cmd.Context(), dir, args[0], ref)
			if err != nil {
				return err
			}
			return reportProjectExplanation(cmd, e)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit to explain (default: the default branch of the Project)")
	return cmd
}

// reportProjectExplanation prints e; an invalid file is also the error of the command.
func reportProjectExplanation(cmd *cobra.Command, e v1.ProjectExplanation) error {
	if e.Invalid != nil {
		invalid := domain.NewError(e.Invalid.Code, e.Invalid.Message, e.Invalid.Details)
		if on, _ := cmd.Flags().GetBool(flagJSON); on {
			if err := encodeJSON(cmd.OutOrStdout(), e); err != nil {
				return err
			}
		}
		return invalid
	}
	return emit(cmd, e, func(w io.Writer) error { return printProjectExplanation(w, e) })
}

func printProjectExplanation(out io.Writer, e v1.ProjectExplanation) error {
	fmt.Fprintf(out, "Project %s at %s · repo_config %s\n", e.Project, e.Commit, e.RepoConfig)
	switch {
	case e.FileIgnored:
		fmt.Fprintln(out, ".codefabric/project.yaml is present and ignored: the Project denies repo configuration.")
	case !e.FilePresent:
		fmt.Fprintln(out, "The commit has no .codefabric/project.yaml.")
	}
	w := newTable(out, "FIELD\tVALUE\tORIGIN")
	for _, f := range e.Fields {
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.Name, valueLabel(f.Value), dash(string(f.Origin)))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if e.EffectiveDigest != "" {
		_, err := fmt.Fprintf(out, "Effective digest: %s\n", e.EffectiveDigest)
		return err
	}
	return nil
}

// valueLabel is a value on one line: text as it is, empty as a dash, the rest as JSON.
func valueLabel(v any) string {
	if s, ok := v.(string); ok {
		return dash(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
