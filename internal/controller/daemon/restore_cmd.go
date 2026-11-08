package daemon

import (
	"encoding/json"
	"fmt"
	"io"
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
		Use:   "restore BACKUP [--advertise HOST:PORT] [--listen HOST:PORT] [--passphrase | --passphrase-file FILE]",
		Short: "Restore a backup into an empty Controller directory",
		Long: "Recreate the Controller of a backup on this host, with the same IDs and digests. It only goes into an\n" +
			"empty data and config directory (see CF_DATA_DIR and CF_CONFIG_DIR) and never starts the daemon.\n" +
			"--advertise sets the address clients and Workers will dial; the server certificate is issued again,\n" +
			"by the restored CA, for it. The daemon binds the backup's --listen, on the advertised port if that\n" +
			"differs from the backup's; --listen sets it. A CA in the backup is opened with --passphrase (asked\n" +
			"for without echo) or --passphrase-file; without a CA in the backup, a new one is made.\n" +
			"\n" +
			"Restore also creates an owner for this host, whose credentials are left in the config directory, so\n" +
			"that you can administer the Controller here at once. Then:\n" +
			"  1. start the Controller:                   cf controller run\n" +
			"  2. with the CA restored, on each client:   cf connect --rebind HOST:PORT\n" +
			"     and on each Worker:                     cf worker set-controller HOST:PORT, then restart it\n" +
			"  3. without the CA, nobody keeps an identity: create invitations here with\n" +
			"     cf controller token create --client|--worker, and join again with cf connect --force <invitation>\n" +
			"     and cf worker join --force <invitation>.\n" +
			"Orders and leases issued after the backup was taken are not in it; the Workers reconcile with what the\n" +
			"Controller holds.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if prompt && passphraseFile != "" {
				return domain.NewError(domain.CodeInvalidRequest, "choose one of --passphrase or --passphrase-file", nil)
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			if err := RequireRestorable(paths); err != nil { // before asking for a passphrase
				return err
			}
			if prompt { // a backup without a CA has no use for the passphrase asked for
				if m, err := readManifest(args[0]); err == nil && m.CA != BackupCAEncrypted {
					prompt = false
				}
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
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			printNextSteps(cmd.OutOrStdout(), res, paths)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&advertise, "advertise", "", "host:port clients dial; its host goes into the new server certificate (default: the backup's)")
	f.StringVar(&listen, "listen", "", "host:port to bind (default: the backup's, on the advertised port if that differs)")
	f.BoolVar(&prompt, "passphrase", false, "open the CA with a passphrase asked for without echo")
	f.StringVar(&passphraseFile, "passphrase-file", "", "open the CA with the passphrase in this file (mode 0600)")
	return cmd
}

// printNextSteps tells the operator what follows a restore, with the real address.
func printNextSteps(out io.Writer, res RestoreResult, paths config.Paths) {
	fmt.Fprintf(out, "Controller %s restored: it will listen on %s and is advertised at %s.\n", res.ControllerID, res.Listen, res.Advertise)
	fmt.Fprintf(out, "An owner for this host was created; its credentials are in %s.\n\nNext:\n", paths.CredentialsDir())
	fmt.Fprintln(out, "  1. Start the Controller:  cf controller run")
	if res.CARestored {
		fmt.Fprintf(out, "  2. On each client:        cf connect --rebind %s\n", res.Advertise)
		fmt.Fprintf(out, "  3. On each Worker:        cf worker set-controller %s   (then restart the Worker)\n", res.Advertise)
		return
	}
	fmt.Fprintln(out, "  2. The backup had no CA, so a new one was made and no old identity is valid. Create an invitation per")
	fmt.Fprintln(out, "     client and per Worker:  cf controller token create --client   |   cf controller token create --worker")
	fmt.Fprintln(out, "  3. On each client:        cf connect --force <invitation>")
	fmt.Fprintln(out, "     On each Worker:        cf worker join --force <invitation>")
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
	if err == nil && len(pass) == 0 {
		return "", domain.NewError(domain.CodeInvalidRequest, "the passphrase is empty", nil)
	}
	return string(pass), err
}
