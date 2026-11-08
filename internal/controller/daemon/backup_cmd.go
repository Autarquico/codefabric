package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// BackupCommand is `cf backup`: a local command, run on the Controller's host
// beside the running daemon. It is not part of `cf controller` because it is
// the operator's, not the daemon's.
func BackupCommand() *cobra.Command {
	var out, passphraseFile string
	var prompt bool
	cmd := &cobra.Command{
		Use:   "backup [--out FILE] [--passphrase | --passphrase-file FILE]",
		Short: "Back up the Controller: database, blobs, relay and settings",
		Long: "Write one file with a consistent copy of this Controller's database, its blobs, its git relay and\n" +
			"controller.json. Credentials and secrets are never included. The CA is included only when you give a\n" +
			"passphrase (--passphrase asks for it without echo; --passphrase-file reads it from a file), encrypted\n" +
			"with it; without it, restoring the backup means every client and Worker joins again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if prompt && passphraseFile != "" {
				return domain.NewError(domain.CodeInvalidRequest, "choose one of --passphrase or --passphrase-file", nil)
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			passphrase, err := readPassphrase(cmd, prompt, passphraseFile)
			if err != nil {
				return err
			}
			if out == "" {
				out = "codefabric-backup-" + time.Now().UTC().Format("20060102T150405Z") + ".tar"
			}
			res, err := Backup(cmd.Context(), BackupOptions{Paths: paths, Out: out, Passphrase: passphrase})
			if err != nil {
				return err
			}
			if !res.CAIncluded {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: the CA is not in this backup, so a Controller restored from it is a new one to every client and Worker: all of them will have to join again. To keep them connected, run `cf backup` again with --passphrase or --passphrase-file.")
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backup written to %s (%d bytes, mode 0600).\n", res.Path, res.Bytes)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&out, "out", "", "file to create; it must not exist (default codefabric-backup-<UTC time>.tar here)")
	f.BoolVar(&prompt, "passphrase", false, "include the CA, encrypted with a passphrase asked for without echo")
	f.StringVar(&passphraseFile, "passphrase-file", "", "include the CA, encrypted with the passphrase in this file (mode 0600)")
	return cmd
}

// readPassphrase returns the passphrase the flags ask for, or "" if they ask
// for none. It is never taken from the arguments or the environment.
func readPassphrase(cmd *cobra.Command, prompt bool, file string) (string, error) {
	switch {
	case file != "":
		return passphraseFromFile(file)
	case prompt:
		return promptPassphrase(cmd)
	}
	return "", nil
}

func passphraseFromFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("--passphrase-file: %v", err), nil)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", domain.NewError(domain.CodeInvalidRequest, path+" is readable by others; make it 0600", nil)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names it
	if err != nil {
		return "", err
	}
	passphrase := strings.TrimRight(string(raw), "\r\n")
	if passphrase == "" {
		return "", domain.NewError(domain.CodeInvalidRequest, path+" is empty", nil)
	}
	return passphrase, nil
}

func promptPassphrase(cmd *cobra.Command) (string, error) {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) { //nolint:gosec // a descriptor fits an int
		return "", domain.NewError(domain.CodeInvalidRequest, "--passphrase needs a terminal; use --passphrase-file", nil)
	}
	ask := func(label string) ([]byte, error) {
		fmt.Fprint(cmd.ErrOrStderr(), label)
		defer fmt.Fprintln(cmd.ErrOrStderr())
		return term.ReadPassword(int(in.Fd())) //nolint:gosec // idem
	}
	first, err := ask("Passphrase for the CA: ")
	if err != nil {
		return "", err
	}
	again, err := ask("Repeat it: ")
	if err != nil {
		return "", err
	}
	if !bytes.Equal(first, again) {
		return "", domain.NewError(domain.CodeInvalidRequest, "the passphrases differ", nil)
	}
	if len(first) == 0 {
		return "", domain.NewError(domain.CodeInvalidRequest, "the passphrase is empty", nil)
	}
	return string(first), nil
}
