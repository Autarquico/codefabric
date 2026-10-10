package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// newWorkerCmd is the administration of Workers. The daemon's own commands
// (join, run) are composed into it by cmd/cf, since they need the Worker packages.
func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Run and administer Workers"}
	cmd.AddCommand(newWorkerListCmd(), newWorkerShowCmd(),
		newWorkerActionCmd("drain", "Stop a Worker from taking new Runs", "Draining", (*client.Client).DrainWorker),
		newWorkerActionCmd("undrain", "Let a drained Worker take Runs again", "Undrained", (*client.Client).UndrainWorker),
		newWorkerActionCmd("remove", "Remove a Worker and revoke its credentials (only without active Runs)", "Removed", (*client.Client).RemoveWorker))
	return cmd
}

func newWorkerListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Workers with their resources and version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			workers, err := c.Workers(cmd.Context(), all)
			if err != nil {
				return err
			}
			if workers == nil {
				workers = []v1.Worker{}
			}
			return emit(cmd, v1.ListWorkersResponse{Workers: workers}, func(out io.Writer) error {
				w := newTable(out, "ID\tNAME\tSTATE\tVERSION\tCPU\tMEMORY\tRUNS\tLAST SEEN")
				for _, worker := range workers {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n", worker.Short, worker.Name, stateOf(worker),
						dash(worker.DaemonVersion), cpuOf(worker), memoryOf(worker),
						worker.ActiveRuns, worker.MaxConcurrentRuns, lastSeen(worker))
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include removed Workers")
	return cmd
}

func newWorkerShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <worker>",
		Short: "Show a Worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			worker, err := c.Worker(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, worker, func(out io.Writer) error { return printWorker(out, worker) })
		},
	}
}

// newWorkerActionCmd is one of drain, undrain and remove: a verb on a Worker
// that answers with the Worker as it is afterwards.
func newWorkerActionCmd(verb, short, done string, call func(*client.Client, context.Context, string) (v1.Worker, error)) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <worker>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			worker, err := call(c, cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, worker, func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "%s %s (%s).\n", done, worker.Name, worker.State)
				return err
			})
		},
	}
}

func stateOf(w v1.Worker) string {
	if w.State == domain.WorkerDegraded && !w.Compatible {
		return string(w.State) + "(version)"
	}
	return string(w.State)
}

func cpuOf(w v1.Worker) string {
	if w.LastHeartbeatAt == nil {
		return "-"
	}
	return fmt.Sprint(w.Resources.Allocatable.CPU)
}

func memoryOf(w v1.Worker) string {
	if w.LastHeartbeatAt == nil {
		return "-"
	}
	return gigabytes(w.Resources.Allocatable.MemoryMB)
}

func gigabytes(mb int) string { return fmt.Sprintf("%.1f GB", float64(mb)/1024) }

func lastSeen(w v1.Worker) string {
	if w.LastHeartbeatAt == nil {
		return "never"
	}
	return local(*w.LastHeartbeatAt)
}

func printWorker(out io.Writer, w v1.Worker) error {
	compatible := "yes"
	if !w.Compatible {
		compatible = "no (outside the upgrade policy)"
	}
	_, err := fmt.Fprintf(out, `ID            %s (%s)
Name          %s
State         %s
Host          %s %s/%s
Version       %s (compatible: %s)
Epoch         %s
Availability  %s
Runs          %d active, at most %d
Total         %s
Allocatable   %s
Free          %s
Health        %s
Capabilities  %s
Last seen     %s
Created       %s
`, w.ID, w.Short, w.Name, stateOf(w), dash(w.Hostname), dash(w.OS), dash(w.Arch), dash(w.DaemonVersion), compatible,
		dash(w.Epoch), w.Availability, w.ActiveRuns, w.MaxConcurrentRuns,
		capacityOf(w.Resources.Total), capacityOf(w.Resources.Allocatable), capacityOf(w.Resources.Free),
		healthOf(w.Health), dash(strings.Join(w.Capabilities, ", ")), lastSeen(w), local(w.CreatedAt))
	return err
}

func capacityOf(c v1.Capacity) string {
	return fmt.Sprintf("%d CPU, %s memory, %s disk", c.CPU, gigabytes(c.MemoryMB), gigabytes(c.DiskMB))
}

func healthOf(h v1.Health) string {
	switch {
	case h.Status == "":
		return "-"
	case h.Message != "":
		return h.Status + ": " + h.Message
	}
	return h.Status
}
