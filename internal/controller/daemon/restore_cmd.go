package daemon

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// RestoreCommand is `cf restore`: a local command, run on the host of the new
// Controller before its daemon starts.
func RestoreCommand() *cobra.Command {
	var advertise, listen, passphraseFile string
	var prompt bool
	cmd := &cobra.Command{
		Use:   "restore BACKUP [--advertise HOST:PORT] [--passphrase | --passphrase-file FILE]",
		Short: "Restore a backup into an empty Controller directory",
		Long: "Recreate the Controller of a backup on this host, with the same IDs and digests. It only goes into an\n" +
			"empty data and config directory (see CF_DATA_DIR and CF_CONFIG_DIR) and never starts the daemon.\n" +
			"--advertise sets the address clients and Workers will dial; the server certificate is issued again,\n" +
			"by the restored CA, for it. A CA in the backup is opened with --passphrase (asked for without echo)\n" +
			"or --passphrase-file; without a CA in the backup, a new one is made and every client and Worker joins again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if prompt && passphraseFile != "" {
				return domain.NewError(domain.CodeInvalidRequest, "choose one of --passphrase or --passphrase-file", nil)
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			passphrase, err := restorePassphrase(cmd, prompt, passphraseFile)
			if err != nil {
				return err
			}
			res, err := Restore(cmd.Context(), RestoreOptions{
				Paths: paths, Backup: args[0], Advertise: advertise, Listen: listen, Passphrase: passphrase,
			})
			if err != nil {
				return err
			}
			if !res.CARestored {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: the backup has no CA, so a new one was made. Every client and Worker must join again.")
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Controller %s restored, advertised at %s. Start it with `cf controller run`.\n", res.ControllerID, res.Advertise)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&advertise, "advertise", "", "host:port clients dial; its host goes into the new server certificate (default: the backup's)")
	f.StringVar(&listen, "listen", "", "address to bind (default: the backup's)")
	f.BoolVar(&prompt, "passphrase", false, "open the CA with a passphrase asked for without echo")
	f.StringVar(&passphraseFile, "passphrase-file", "", "open the CA with the passphrase in this file (mode 0600)")
	return cmd
}

// restorePassphrase is readPassphrase, but asks once: a wrong passphrase is refused anyway.
func restorePassphrase(cmd *cobra.Command, prompt bool, file string) (string, error) {
	if !prompt {
		return readPassphrase(cmd, false, file)
	}
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) { //nolint:gosec // a descriptor fits an int
		return "", domain.NewError(domain.CodeInvalidRequest, "--passphrase needs a terminal; use --passphrase-file", nil)
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Passphrase of the CA: ")
	defer fmt.Fprintln(cmd.ErrOrStderr())
	pass, err := term.ReadPassword(int(in.Fd())) //nolint:gosec // idem
	return string(pass), err
}
