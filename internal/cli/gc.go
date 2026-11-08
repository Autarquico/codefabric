package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func newGCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Collect garbage in the Controller: expired history and blobs nothing refers to",
		Long: "Runs a pass of the garbage collector of the Controller (not of the Workers, which clean their own " +
			"machines). With --dry-run it lists what the pass would delete and changes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			rep, err := c.GC(cmd.Context(), dryRun)
			if err != nil {
				return err
			}
			return emit(cmd, rep, func(out io.Writer) error { return printGC(out, rep) })
		},
	}
	cmd.Flags().Bool("dry-run", false, "list what would be deleted, and delete nothing")
	return cmd
}

func printGC(out io.Writer, rep v1.GCReport) error {
	verb := "Deleted"
	if rep.DryRun {
		verb = "Would delete"
		fmt.Fprintln(out, "Dry run: nothing was deleted.")
	}
	return printItems(out, verb, rep)
}

func printItems(out io.Writer, verb string, rep v1.GCReport) error {
	w := newTable(out, verb+"\tCOUNT\tBYTES")
	for _, it := range rep.Items {
		bytes := "-"
		if it.Bytes > 0 {
			bytes = byteSize(it.Bytes)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", it.Kind, it.Count, bytes)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if rep.HeldBack > 0 {
		_, err := fmt.Fprintf(out, "%d events past their retention stay: a Run that has not ended, or an event before them, holds them.\n", rep.HeldBack)
		return err
	}
	return nil
}

func newStorageCmd() *cobra.Command {
	storage := &cobra.Command{Use: "storage", Short: "Inspect what the Controller stores"}
	storage.AddCommand(&cobra.Command{
		Use:   "stats",
		Short: "Show the bytes the Controller stores by category, and how much a GC pass would free",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			st, err := c.StorageStats(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, st, func(out io.Writer) error { return printStorageStats(out, st) })
		},
	})
	return storage
}

func printStorageStats(out io.Writer, st v1.StorageStats) error {
	w := newTable(out, "CATEGORY\tBYTES")
	for _, row := range []struct {
		name  string
		bytes int64
	}{
		{"output blobs", st.OutputBlobBytes},
		{"workflow blobs", st.WorkflowBlobBytes},
		{"unreferenced blobs", st.UnreferencedBlobBytes},
		{"database", st.DatabaseBytes},
		{"git relay", st.RelayBytes},
		{"total", st.TotalBytes},
	} {
		fmt.Fprintf(w, "%s\t%s\n", row.name, byteSize(row.bytes))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if st.MaxStorageBytes > 0 {
		state := "within"
		if st.OverMaxStorage {
			state = "over"
		}
		fmt.Fprintf(out, "max_storage %s: %s it (the GC only warns).\n", byteSize(st.MaxStorageBytes), state)
	}
	fmt.Fprintln(out, "\nA GC pass would free:")
	return printItems(out, "Would delete", st.Reclaimable)
}

// byteSize writes n in the largest binary unit that keeps it above 1.
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), ""
	for _, s := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		suffix = s
		if value < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
