package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/envutil"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// DoctorResult is what `cf worker doctor --json` prints.
type DoctorResult struct {
	Components []v1.ComponentHealth `json:"components"`
}

// DoctorOptions parameterise RunDoctor.
type DoctorOptions struct {
	Paths       config.Paths
	DiskMinFree string
	// Doctor overrides the checks, for tests.
	Doctor doctor.Options
}

// RunDoctor runs the checks of this machine, with no daemon and no Controller.
func RunDoctor(ctx context.Context, o DoctorOptions) ([]v1.ComponentHealth, error) {
	minFree, err := doctor.MinFree(o.Paths, o.DiskMinFree)
	if err != nil {
		return nil, err
	}
	opts := o.Doctor
	if opts.Host == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if opts.Host, err = localhost.New(localhost.Options{Home: home, Env: envutil.WithoutTmux(os.Environ())}); err != nil {
			return nil, err
		}
	}
	if opts.Adapters == nil {
		opts.Adapters = []harness.Adapter{command.Adapter{}, codex.Adapter{}, claude.Adapter{}}
	}
	if opts.Worker == "" {
		hostname, _ := os.Hostname()
		opts.Worker = doctor.WorkerName(o.Paths, workerName(hostname))
	}
	if opts.DataDir == "" {
		opts.DataDir = o.Paths.WorkerDataDir()
	}
	if opts.Secrets == nil {
		opts.Secrets = secretsfile.New(o.Paths.WorkerSecretsFile(), clock.Real{})
	}
	if opts.MinFree == (doctor.Threshold{}) {
		opts.MinFree = minFree
	}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	return doctor.New(opts).Check(ctx), nil
}

func newDoctorCmd() *cobra.Command {
	var minFree string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine as a Worker: tmux, git, disk, harnesses and their logins",
		Long: "Check what this machine needs to run Runs, without the daemon and without a Controller.\n" +
			"It exits non-zero if any component fails. A login is only checked to be there: its content\n" +
			"is never read nor printed. With --json and a failure, the §40 error envelope carries the\n" +
			"components in its details.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			components, err := RunDoctor(cmd.Context(), DoctorOptions{Paths: paths, DiskMinFree: minFree})
			if err != nil {
				return err
			}
			return printDoctor(cmd, components)
		},
	}
	cmd.Flags().StringVar(&minFree, "disk-min-free", "", "fail below this free disk, 20% or 50GiB (default: the Worker config, then 20%)")
	return cmd
}

func printDoctor(cmd *cobra.Command, components []v1.ComponentHealth) error {
	failing := doctor.Failing(components)
	failure := domain.NewError(domain.CodeInternal, fmt.Sprintf("doctor: %d component(s) fail", failing),
		map[string]any{"components": components})
	if jsonOutput(cmd) {
		if failing > 0 {
			return failure
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(DoctorResult{Components: components})
	}
	if err := writeDoctorTable(cmd.OutOrStdout(), components); err != nil {
		return err
	}
	if failing > 0 {
		failure.Details = nil
		return failure
	}
	return nil
}

func writeDoctorTable(out io.Writer, components []v1.ComponentHealth) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "COMPONENT\tSTATUS\tMESSAGE\tACTION")
	for _, c := range components {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.Status, c.Message, c.Action)
	}
	return w.Flush()
}
