package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
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
	// Controller is the host:port to reach instead of the stored one.
	Controller string
	// Clock measures the deadlines; nil is the system's.
	Clock clock.Clock
	// Doctor overrides the checks, for tests.
	Doctor doctor.Options
	// Probe replaces the authenticated call to the Controller, for tests.
	Probe func(ctx context.Context) error
}

// controllerTimeout bounds the call to the Controller, which may not answer.
const controllerTimeout = 5 * time.Second

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
		opts.Adapters = doctor.Offered(o.Paths, []harness.Adapter{command.Adapter{}, codex.Adapter{}, claude.Adapter{}})
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
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	cctx, cancel := withDeadline(ctx, clk, doctorTimeout)
	defer cancel()
	components := doctor.New(opts).Check(cctx)
	return append(components, controllerComponent(ctx, clk, o, opts.Worker)), nil
}

// controllerComponent says whether this Worker can talk to its Controller with
// the credentials it has: an authenticated read that changes nothing. A machine
// that has not joined only warns, since the checks of RunDoctor do not need it;
// credentials the Controller refuses fail, as no Run could reach this Worker.
func controllerComponent(ctx context.Context, clk clock.Clock, o DoctorOptions, worker string) v1.ComponentHealth {
	probe := o.Probe
	if probe == nil {
		exists, err := client.CredentialsExist(o.Paths.WorkerCredentialsDir())
		switch {
		case err != nil:
			return doctor.CleanComponent(v1.ComponentHealth{
				Name: "controller", Status: v1.ComponentFail, Message: "no se pueden leer las credenciales: " + err.Error(),
				Action: "revisa " + o.Paths.WorkerCredentialsDir() + " en " + worker,
			})
		case !exists:
			return v1.ComponentHealth{
				Name: "controller", Status: v1.ComponentWarn, Message: "este equipo no se ha unido a un Controller",
				Action: "cf worker join <invitación> en " + worker,
			}
		}
		opts := []client.Option{client.WithRecoveryHint(client.WorkerRecoveryHint)}
		if o.Controller != "" {
			opts = append(opts, client.WithController(o.Controller))
		}
		c, err := client.Open(o.Paths.WorkerCredentialsDir(), opts...)
		if err != nil {
			return doctor.CleanComponent(v1.ComponentHealth{
				Name: "controller", Status: v1.ComponentFail, Message: "no se pueden abrir las credenciales: " + err.Error(),
				Action: "cf worker join --force <invitación> en " + worker,
			})
		}
		probe = func(ctx context.Context) error { _, err := c.Assignments(ctx, 0, 0, 1); return err }
	}
	cctx, cancel := withDeadline(ctx, clk, controllerTimeout)
	defer cancel()
	if err := probe(cctx); err != nil {
		return doctor.CleanComponent(v1.ComponentHealth{
			Name: "controller", Status: v1.ComponentFail, Message: "el Controller no acepta a este Worker: " + err.Error(),
			Action: "revisa la red y las credenciales de " + worker + "; " + client.WorkerRecoveryHint,
		})
	}
	return v1.ComponentHealth{Name: "controller", Status: v1.ComponentOK, Message: "el Controller responde y acepta las credenciales"}
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
			controller, _ := cmd.Flags().GetString("controller")
			components, err := RunDoctor(cmd.Context(), DoctorOptions{Paths: paths, DiskMinFree: minFree, Controller: controller})
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
