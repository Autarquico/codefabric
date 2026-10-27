package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// The secrets of this Worker are local: `cf worker secrets` writes the catalog
// and talks to no one, and the next heartbeat tells the Controller their refs.
// A value reaches the command only on stdin or at a prompt, never in argv, and
// no message repeats what the command was given: an argument may be a value
// typed in the wrong place.

// valueFlag exists only to refuse a value given as a flag, and say why.
const valueFlag = "value"

// newSecretsCmd is `cf worker secrets`.
func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the secrets this Worker holds for Runs",
		Long: "Manage the secrets this Worker holds for the Runs that use them. Values stay on this\n" +
			"machine; the Controller learns only their refs, at the next heartbeat. A value is read\n" +
			"from stdin, or typed at a prompt: never from an argument, which every user of the\n" +
			"machine can see.",
		// Runnable, so that an unknown subcommand is refused here, without
		// repeating it.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 || jsonOutput(cmd) {
				return invalid("`cf worker secrets` takes a subcommand: set, list or rm")
			}
			return cmd.Help()
		},
	}
	// pflag quotes what it could not parse, which may be a value.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, _ error) error {
		return invalid(fmt.Sprintf("a flag of `%s` is unknown or lacks its value; see --help", c.CommandPath()))
	})
	cmd.AddCommand(newSecretsSetCmd(), newSecretsListCmd(), newSecretsRmCmd())
	return cmd
}

func newSecretsSetCmd() *cobra.Command {
	var projects []string
	cmd := &cobra.Command{
		Use:   "set <ref>",
		Short: "Store a secret under ref, reading its value from stdin or a prompt",
		Long: "Store a secret under ref, replacing what ref held. The value is read from stdin up to\n" +
			"its end, without one final newline, or, on a terminal, typed twice without echo. It\n" +
			"has " + fmt.Sprint(domain.MinSecretValueLen) + " to " + fmt.Sprint(domain.MaxSecretValueLen) +
			" bytes: a shorter one could not be redacted from the output of\na Run. --project limits it to the Runs of those Projects.",
		Example: "  printf %s \"$TOKEN\" | cf worker secrets set github/token\n  cf worker secrets set npm/token --project project_01J...",
		// The arguments are checked in RunE, to explain a value given as one.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed(valueFlag) {
				return invalid("a value is never a flag: arguments are visible to every user of this machine " +
					"and stay in the shell's history. Pipe it on stdin or type it at the prompt; nothing was stored")
			}
			if len(args) != 1 {
				return invalid("give one ref; its value goes on stdin or at the prompt, never as an argument. Nothing was stored")
			}
			ref := args[0]
			if err := checkSecretArgs(ref, projects); err != nil {
				return err
			}
			store, err := openSecrets()
			if err != nil {
				return err
			}
			value, err := readValue(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), ref)
			if err != nil {
				return err
			}
			defer clear(value)
			if err := cmd.Context().Err(); err != nil {
				return cancelled()
			}
			if err := store.Set(cmd.Context(), ref, value, projects); err != nil {
				return secretsError(err)
			}
			meta, err := secretMeta(cmd.Context(), store, ref)
			if err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(meta)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stored secret %s (%s) for %s. %s\n", ref, meta.Backend, scopeLabel(meta.Projects), nextHeartbeat)
			return err
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&projects, "project", nil, "only Runs of this Project may use it, by Project ID; repeatable (default: any Project)")
	f.String(valueFlag, "", "")
	f.Lookup(valueFlag).NoOptDefVal = " "
	_ = f.MarkHidden(valueFlag)
	return cmd
}

func newSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the secrets of this Worker: refs and metadata, never values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openSecrets()
			if err != nil {
				return err
			}
			metas, err := store.List(cmd.Context())
			if err != nil {
				return secretsError(err)
			}
			list := make([]v1.SecretMeta, 0, len(metas))
			for _, m := range metas {
				list = append(list, toSecretMeta(m))
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Secrets []v1.SecretMeta `json:"secrets"`
				}{list})
			}
			return printSecrets(cmd.OutOrStdout(), list)
		},
	}
}

func newSecretsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <ref>",
		Short: "Remove a secret from this Worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if err := checkSecretArgs(ref, nil); err != nil {
				return err
			}
			store, err := openSecrets()
			if err != nil {
				return err
			}
			if err = store.Delete(cmd.Context(), ref); err != nil {
				return secretsError(err)
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Ref     string `json:"ref"`
					Removed bool   `json:"removed"`
				}{ref, true})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed secret %s. %s\n", ref, nextHeartbeat)
			return err
		},
	}
}

var nextHeartbeat = fmt.Sprintf("The Controller will know at the next heartbeat (every %s).", domain.Defaults().WorkerHeartbeatInterval)

func openSecrets() (*secretsfile.Store, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	return secretsfile.New(paths.WorkerSecretsFile(), clock.Real{}), nil
}

// checkSecretArgs vets ref and projects before a value is read, with messages
// that do not repeat them.
func checkSecretArgs(ref string, projects []string) error {
	if !domain.ValidSecretRef(ref) {
		return invalid(fmt.Sprintf("the ref is not valid: a letter or digit followed by letters, digits, '.', '_', '/' or '-', up to %d", domain.MaxSecretRefLen))
	}
	if len(projects) > domain.MaxSecretProjects {
		return invalid(fmt.Sprintf("a secret is scoped to at most %d Projects", domain.MaxSecretProjects))
	}
	for _, p := range projects {
		if kind, _, err := domain.ParseID(p); err != nil || kind != domain.KindProject {
			return invalid("--project takes a Project ID (project_<ULID>, as `cf project show` gives it)")
		}
	}
	return nil
}

// readValue is the value of a secret: typed twice at a prompt when in is a
// terminal, else in up to its end, without one final newline. It ends early,
// storing nothing, when ctx does.
func readValue(ctx context.Context, in io.Reader, prompt io.Writer, ref string) ([]byte, error) {
	if tty, ok := in.(*os.File); ok && isTerminal(tty) {
		return promptValue(ctx, tty, prompt, ref)
	}
	read := make(chan []byte, 1)
	failed := make(chan error, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(in, domain.MaxSecretValueLen+2))
		if err != nil {
			failed <- err
			return
		}
		read <- raw
	}()
	select {
	case raw := <-read:
		value, _ := bytes.CutSuffix(raw, []byte("\n"))
		return value, checkValue(value)
	case err := <-failed:
		return nil, fmt.Errorf("reading the value from stdin: %w", err)
	case <-ctx.Done():
		return nil, cancelled()
	}
}

// checkValue is domain.CheckSecretValue with a message of its own: a value cut
// at the limit has no size to tell.
func checkValue(value []byte) error {
	switch {
	case len(value) == 0:
		return invalid("no value given: pipe it on stdin or run the command on a terminal to type it. Nothing was stored")
	case len(value) < domain.MinSecretValueLen:
		return invalid(fmt.Sprintf("the value has fewer than %d bytes and could not be redacted from the output of a Run; nothing was stored", domain.MinSecretValueLen))
	case len(value) > domain.MaxSecretValueLen:
		return invalid(fmt.Sprintf("the value has more than %d bytes; nothing was stored", domain.MaxSecretValueLen))
	}
	return nil
}

// secretsError keeps the arguments out of what the Store says: its errors may
// quote a ref.
func secretsError(err error) error {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return domain.NewError(domain.CodeNotFound, "this Worker holds no secret by that ref; see `cf worker secrets list`", nil)
	case errors.Is(err, domain.ErrInvalidSecret):
		return invalid("the secret is not valid; nothing was stored")
	case errors.Is(err, context.Canceled):
		return cancelled()
	}
	return err
}

func secretMeta(ctx context.Context, store secrets.Store, ref string) (v1.SecretMeta, error) {
	metas, err := store.List(ctx)
	if err != nil {
		return v1.SecretMeta{}, secretsError(err)
	}
	for _, m := range metas {
		if m.Ref == ref {
			return toSecretMeta(m), nil
		}
	}
	return v1.SecretMeta{}, secretsError(secrets.ErrNotFound)
}

func toSecretMeta(m secrets.Metadata) v1.SecretMeta {
	return v1.SecretMeta{Ref: m.Ref, Backend: m.Backend, Projects: m.Projects, UpdatedAt: m.UpdatedAt}
}

func printSecrets(out io.Writer, list []v1.SecretMeta) error {
	if len(list) == 0 {
		_, err := fmt.Fprintln(out, "This Worker holds no secrets. Add one with: cf worker secrets set <ref>")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REF\tBACKEND\tPROJECTS\tUPDATED")
	for _, m := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Ref, m.Backend, scopeLabel(m.Projects), m.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func scopeLabel(projects []string) string {
	if len(projects) == 0 {
		return "any Project"
	}
	return strings.Join(projects, ", ")
}

func invalid(message string) error { return domain.NewError(domain.CodeInvalidRequest, message, nil) }

func cancelled() error { return invalid("cancelled; nothing was stored") }
