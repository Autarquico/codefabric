package cli

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client/notify"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// NewNotifier builds the Notifier of this machine: the preferences of the
// client config over the adapters the binary composed in. The TUI starts it in
// process (cmd/cf wires it); `cf notify watch` runs it in the foreground.
func NewNotifier(native notify.Native, log *slog.Logger) (*notify.Notifier, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadClient(paths)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	return notify.New(notify.Options{Prefs: cfg.Notify, Native: native, Log: log}), nil
}

func newNotifyCmd(native notify.Native) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Desktop notifications and hooks for the events that need you",
		Long: "Notifications are made by this process, on this machine, from the event stream; the Controller\n" +
			"does not send them. The TUI notifies on its own while it is open. What notifies is set under \"notify\" in client.json (see `cf config paths`):\n\n" +
			"  {\"notify\": {\n" +
			"    \"events\": [\"RunFailed\", \"TaskNeedsInput\", \"WorkerLost\"],   default; any event type, [] for none\n" +
			"    \"min_level\": \"warn\",                                       least severe level: debug, info, warn, error\n" +
			"    \"muted_projects\": [\"<project id>\"],\n" +
			"    \"quiet_hours\": {\"start\": \"22:00\", \"end\": \"07:30\"},      local time; holds back native notifications only\n" +
			"    \"hook\": [\"/path/to/command\", \"arg\"]                         argv, no shell; gets each event as JSON on stdin\n" +
			"  }}\n\n" +
			"The hook gets the event as JSON (type, run_id, task_id, level, message, ...) and has 10 s to finish.",
	}
	cmd.AddCommand(newNotifyWatchCmd(native), newNotifyTestCmd(native))
	return cmd
}

func newNotifyWatchCmd(native notify.Native) *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Notify about events as they happen, until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := NewNotifier(native, stderrLogger(cmd.ErrOrStderr()))
			if err != nil {
				return err
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "watching for: %v\n", n.Types())
			return n.Watch(cmd.Context(), c)
		},
	}
}

func newNotifyTestCmd(native notify.Native) *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Send a sample notification, through the native channel and the hook",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := NewNotifier(native, stderrLogger(cmd.ErrOrStderr()))
			if err != nil {
				return err
			}
			if err := n.Test(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "sample notification sent")
			return nil
		},
	}
}

func stderrLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
