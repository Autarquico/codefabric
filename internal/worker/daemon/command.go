package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/support/svcunit"
	"github.com/Autarquico/codefabric/internal/worker/envwrap"
	"github.com/Autarquico/codefabric/internal/worker/executor"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

// Command is the part of `cf worker` that is the daemon's: join and run. The
// CLI merges it with the administration commands of the same name.
func Command() *cobra.Command {
	cmd := &cobra.Command{Use: "worker"}
	cmd.AddCommand(newJoinCmd(), newRunCmd(), newExecCmd(), newRedactPipeCmd(), newInstallServiceCmd(), newUninstallServiceCmd(), newSecretsCmd())
	return cmd
}

func newJoinCmd() *cobra.Command {
	var (
		name, memoryMax, diskMax, availability string
		settings                               v1.WorkerSettings
		force                                  bool
	)
	cmd := &cobra.Command{
		Use:   "join <invitation | ->",
		Short: "Enrol this machine as a Worker using an invitation",
		Long: "Enrol this machine as a Worker. The invitation is an argument or `-` to read it from\n" +
			"stdin, which keeps the token out of shell history. The Worker's credentials are kept\n" +
			"apart from the client's, so one machine can be both.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			invitation, err := readInvitation(cmd.InOrStdin(), args[0])
			if err != nil {
				return err
			}
			if settings.MemoryMaxMB, err = parseSizeMB("--memory-max", memoryMax); err != nil {
				return err
			}
			if settings.DiskMaxMB, err = parseSizeMB("--disk-max", diskMax); err != nil {
				return err
			}
			settings.Availability = domain.AvailabilityPolicy(availability)
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			if name == "" {
				hostname, _ := os.Hostname()
				name = workerName(hostname)
			}
			res, err := Join(cmd.Context(), JoinOptions{
				Paths: paths, Invitation: invitation, Name: name, Settings: settings, Force: force,
			})
			if err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					IdentityID string `json:"identity_id"`
					Controller string `json:"controller"`
					Name       string `json:"name"`
				}{res.IdentityID, res.Controller, name})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Joined %s as Worker %s (%s). Start it with: cf worker run\n", res.Controller, name, res.IdentityID)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of this Worker (default: the hostname, as a slug)")
	f.IntVar(&settings.CPUMax, "cpu-max", 0, "CPUs to contribute (default: all)")
	f.StringVar(&memoryMax, "memory-max", "", "memory to contribute, e.g. 16GB (default: all)")
	f.StringVar(&diskMax, "disk-max", "", "disk to contribute, e.g. 100GB (default: all)")
	f.IntVar(&settings.MaxConcurrentRuns, "max-concurrent", 0, "Runs at the same time (default: "+strconv.Itoa(domain.Defaults().WorkerMaxConcurrentRuns)+")")
	f.StringVar(&availability, "availability", "", "when to accept Runs: always, idle or manual (default: always)")
	f.BoolVar(&force, "force", false, "replace the Worker credentials already stored on this machine")
	return cmd
}

func newRunCmd() *cobra.Command {
	var (
		interval  time.Duration
		sshTarget string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the Worker daemon until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			controller, _ := cmd.Flags().GetString("controller")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return Run(ctx, Options{
				Paths: paths, Controller: controller, Interval: interval, SSHTarget: sshTarget,
				Logger: slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
		},
	}
	// Tests of a run_lost_timeout of seconds need heartbeats closer than Appendix A's.
	cmd.Flags().StringVar(&sshTarget, "ssh-target", "",
		"how clients reach this machine to attach to a Run, [user@]host[:port] (default: <user>@<hostname>; none: attach is not offered)")
	cmd.Flags().DurationVar(&interval, "heartbeat-interval", 0, "time between heartbeats (default: that of Appendix A)")
	_ = cmd.Flags().MarkHidden("heartbeat-interval")
	return cmd
}

// newExecCmd is the wrapper the Runtime starts in the session of a Run (§6.4).
// It is the Worker's, not a person's, so it stays out of the help.
func newExecCmd() *cobra.Command {
	var runDir string
	cmd := &cobra.Command{
		Use:    "exec",
		Short:  "Run the harness of a Run under the wrapper (started by the Worker, not by hand)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			release, err := executor.HoldCapture(runDir)
			if err != nil {
				return err
			}
			defer release()
			return execwrap.Run(execwrap.Options{
				RunDir: runDir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
				Secrets: secretjit.Resolver(runDir), Retention: retention(runDir),
			})
		},
	}
	cmd.Flags().StringVar(&runDir, "run-dir", "", "the directory of the Run, with its launch.json")
	_ = cmd.MarkFlagRequired("run-dir")
	cmd.AddCommand(newExecEnvCmd())
	return cmd
}

// newExecEnvCmd is the supervisor of the Environment of a Run (§25): it
// prepares the worktree and runs the services. The Worker starts it, in a
// process group of its own.
func newExecEnvCmd() *cobra.Command {
	var runDir string
	cmd := &cobra.Command{
		Use:    "env",
		Short:  "Prepare the Environment of a Run and run its services (started by the Worker, not by hand)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			release, err := executor.HoldCapture(runDir)
			if err != nil {
				return err
			}
			defer release()
			return envwrap.Run(envwrap.Options{RunDir: runDir, Secrets: secretjit.Resolver(runDir), Retention: retention(runDir)})
		},
	}
	cmd.Flags().StringVar(&runDir, "run-dir", "", "the directory of the Run, with its env-launch.json")
	_ = cmd.MarkFlagRequired("run-dir")
	return cmd
}

// newRedactPipeCmd is the capture filter of the terminal of an interactive
// Run with secrets: the Runtime starts it, not a person.
func newRedactPipeCmd() *cobra.Command {
	var runDir string
	cmd := &cobra.Command{
		Use:    "redact-pipe",
		Short:  "Copy the terminal of a Run to its log with its secrets redacted (started by the Worker, not by hand)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			release, err := executor.HoldCapture(runDir)
			if err != nil {
				return err
			}
			defer release()
			return execwrap.RedactPipe(execwrap.PipeOptions{RunDir: runDir, Retention: retention(runDir), In: os.Stdin, Out: os.Stdout})
		},
	}
	cmd.Flags().StringVar(&runDir, "run-dir", "", "the directory of the Run, with its secrets")
	_ = cmd.MarkFlagRequired("run-dir")
	return cmd
}

// retention is what the output of the Run in runDir is redacted of.
func retention(runDir string) execwrap.LoadSecrets {
	return func() ([]redact.Needle, error) { return secretjit.Load(runDir) }
}

func newInstallServiceCmd() *cobra.Command {
	var (
		o      ServiceOptions
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "install-service",
		Short: "Run the Worker as a service of this user (systemd --user or launchd)",
		Long: "Run `cf worker run` as a service of this user: a systemd --user unit on Linux, a launchd\n" +
			"agent on macOS. The service keeps the Runs alive across its own restarts, and installing\n" +
			"what is installed and running changes nothing. Its PATH is calculated now (--path replaces\n" +
			"it); changing it later does not reach the Runs that are going, nor a tmux server already up.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			o.Paths, o.TmuxTmpDir = paths, os.Getenv("TMUX_TMPDIR")
			o.Controller, _ = cmd.Flags().GetString("controller")
			if dryRun {
				path, content, warnings, err := RenderService(o)
				if err != nil {
					return err
				}
				return svcunit.PrintFile(cmd.OutOrStdout(), cmd.ErrOrStderr(), jsonOutput(cmd), path, content, warnings)
			}
			res, err := InstallService(cmd.Context(), o)
			if err != nil {
				return err
			}
			return printService(cmd, res, "Installed the Worker service", "The Worker service is installed and running; nothing changed")
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.SSHTarget, "ssh-target", "",
		"how clients reach this machine to attach to a Run, [user@]host[:port] (default: <user>@<hostname>; none: attach is not offered)")
	f.StringVar(&o.Binary, "binary", "", "the cf the service runs, as an absolute path (default: this one)")
	f.StringVar(&o.PATH, "path", "", "the PATH of the service, of absolute directories (default: those of cf, tmux and git, then the usual ones)")
	f.BoolVar(&dryRun, "dry-run", false, "show the file and where it would go, and change nothing")
	return cmd
}

func newUninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-service",
		Short: "Stop and remove the Worker's service; its Runs go on",
		Long: "Stop the Worker's service, disable it and remove its file. The Runs in tmux are not\n" +
			"stopped: they go on unsupervised until the Worker runs again, and the command lists them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := UninstallService(cmd.Context(), ServiceOptions{})
			if err != nil {
				return err
			}
			return printService(cmd, res, "Uninstalled the Worker service", "No Worker service is installed; nothing changed")
		},
	}
}

func printService(cmd *cobra.Command, res ServiceResult, changed, unchanged string) error {
	if jsonOutput(cmd) {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}
	return svcunit.PrintResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), res.Result, changed, unchanged)
}

// jsonOutput reads the root command's persistent --json (internal/cli owns
// it; this package cannot import the CLI).
func jsonOutput(cmd *cobra.Command) bool {
	on, _ := cmd.Flags().GetBool("json")
	return on
}

func readInvitation(stdin io.Reader, arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", domain.NewError(domain.CodeInvalidRequest, "no invitation given", nil)
	}
	return strings.TrimSpace(line), nil
}

// workerName turns a hostname into a Worker name: its first label, in lower
// case, with anything a name cannot hold replaced by '-'.
func workerName(hostname string) string {
	label, _, _ := strings.Cut(strings.ToLower(hostname), ".")
	slug := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, label), "-_")
	if slug == "" {
		return "worker"
	}
	return slug
}

// parseSizeMB reads "512MB", "16GB" or "2TB" (powers of 1024) as megabytes;
// empty means no limit.
func parseSizeMB(flag, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	bad := domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s must be a size such as 512MB, 16GB or 2TB, got %q", flag, text), nil)
	upper := strings.ToUpper(text)
	for suffix, factor := range map[string]int{"MB": 1, "GB": 1 << 10, "TB": 1 << 20} {
		digits, found := strings.CutSuffix(upper, suffix)
		if !found {
			continue
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n <= 0 || n > (1<<31-1)/factor {
			return 0, bad
		}
		return n * factor, nil
	}
	return 0, bad
}
