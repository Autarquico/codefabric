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

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Command is the part of `cf worker` that is the daemon's: join and run. The
// CLI merges it with the administration commands of the same name.
func Command() *cobra.Command {
	cmd := &cobra.Command{Use: "worker"}
	cmd.AddCommand(newJoinCmd(), newRunCmd())
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
	return &cobra.Command{
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
				Paths: paths, Controller: controller,
				Logger: slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
		},
	}
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
