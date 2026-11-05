package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

const (
	flagKind  = "kind"
	flagLimit = "limit"
)

func newSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find Projects, Tasks, Runs, Workers and Workflows by reference or by text",
		Long: "Search resolves the query as a reference first (short ID, ULID prefix, name) and lists " +
			"every candidate when it is ambiguous, then lists the entities whose text matches, best first.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			kind, _ := cmd.Flags().GetString(flagKind)
			limit, _ := cmd.Flags().GetInt(flagLimit)
			res, err := c.Search(cmd.Context(), client.SearchQuery{
				Text: strings.Join(args, " "), Kind: kind, Project: refScope(cmd, c), Limit: limit,
			})
			if err != nil {
				return err
			}
			return emit(cmd, res, func(out io.Writer) error { return printSearch(out, res.Results) })
		},
	}
	cmd.Flags().String(flagKind, "", "only this kind: project, task, run, worker or workflow")
	cmd.Flags().Int(flagLimit, 0, "how many results (default 20, at most 100)")
	addProjectFlag(cmd, "only what belongs to this Project")
	return cmd
}

func printSearch(out io.Writer, results []v1.SearchResult) error {
	w := newTable(out, "KIND\tID\tNAME\tMATCH")
	for _, r := range results {
		id := r.Short
		if id == "" {
			id = r.ID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Kind, printable(id), printable(r.Name), r.Match)
	}
	return w.Flush()
}
