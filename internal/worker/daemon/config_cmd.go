package daemon

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change the settings of this Worker that stay on this machine",
	}
	cmd.AddCommand(newConfigShowCmd(), newConfigSetCmd())
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the settings of this Worker",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			settings, err := doctor.LoadSettings(paths)
			if err != nil {
				return err
			}
			out, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", out)
			return err
		},
	}
}

// settingKeys are the settings `cf worker config set` changes, each with the
// check of its value and where it goes.
var settingKeys = map[string]func(s *doctor.Settings, value string) error{
	"disk-min-free": func(s *doctor.Settings, value string) error {
		if _, err := doctor.ParseThreshold(value); err != nil {
			return err
		}
		s.DiskMinFree = value
		return nil
	},
	"allow_shell": func(s *doctor.Settings, value string) error {
		switch value {
		case "true":
			s.AllowShell = true
		case "false":
			s.AllowShell = false
		default:
			return domain.NewError(domain.CodeInvalidRequest, "allow_shell is true or false, not "+strconv.Quote(value), nil)
		}
		return nil
	},
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change a setting of this Worker: disk-min-free, allow_shell",
		Long: "Change a setting of this Worker, keeping the others. disk-min-free is the free disk below\n" +
			"which the doctor fails, a percentage such as 20% or a size such as 50GiB. allow_shell (true or false,\n" +
			"false by default) lets `cf shell` open a shell in the worktree of a Run on this Worker, as the user the\n" +
			"Worker runs as; the Project must allow it too. Turning it off takes effect at once, turning it on at the\n" +
			"next start of the Worker.",
		Example: "  cf worker config set disk-min-free 10%\n  cf worker config set allow_shell true",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			apply, ok := settingKeys[args[0]]
			if !ok {
				return domain.NewError(domain.CodeInvalidRequest, "the settings that can be set are: "+strings.Join(slices.Sorted(maps.Keys(settingKeys)), ", "), nil)
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			settings, err := doctor.LoadSettings(paths)
			if err != nil {
				return err
			}
			if err := apply(&settings, args[1]); err != nil {
				return err
			}
			if err := doctor.SaveSettings(paths, settings); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Set %s to %s.\n", args[0], args[1])
			return err
		},
	}
}
