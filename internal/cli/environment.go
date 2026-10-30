package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// maxEnvironmentFile bounds the definition file the CLI reads; a definition
// within the domain's limits is far smaller.
const maxEnvironmentFile = 1 << 20

// environmentStore is the part of *client.Client the environment commands use.
type environmentStore interface {
	ListEnvironments(ctx context.Context) ([]v1.EnvironmentSummary, error)
	ShowEnvironment(ctx context.Context, ref string) (v1.ShowEnvironmentResponse, error)
	ApplyEnvironment(ctx context.Context, name string, req v1.ApplyEnvironmentRequest) (v1.EnvironmentVersion, error)
}

func openEnvironmentStore(opts Options) func(*cobra.Command) (environmentStore, error) {
	if opts.environmentStore != nil {
		return opts.environmentStore
	}
	return func(cmd *cobra.Command) (environmentStore, error) { return openClient(cmd) }
}

func newEnvironmentCmd(open func(*cobra.Command) (environmentStore, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "environment", Short: "Apply and inspect Environments (how a worktree is prepared for a Run)"}
	cmd.AddCommand(newEnvironmentApplyCmd(open), newEnvironmentListCmd(open), newEnvironmentShowCmd(open))
	return cmd
}

// environmentFile is the definition file: an Environment definition and,
// optionally, the name it is applied to.
type environmentFile struct {
	Name string `json:"name"`
	domain.EnvironmentDef
}

// decodeEnvironmentFile reads a definition file, YAML or JSON, strictly: a
// field it does not know, or a second document, is an error. The YAML is
// carried to JSON so that the json names of domain.EnvironmentDef apply.
func decodeEnvironmentFile(raw []byte) (environmentFile, error) {
	ydec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc any
	if err := ydec.Decode(&doc); err != nil {
		return environmentFile{}, fmt.Errorf("the definition file is not valid YAML: %w", err)
	}
	var extra any
	if err := ydec.Decode(&extra); !errors.Is(err, io.EOF) {
		return environmentFile{}, errors.New("the definition file has more than one document")
	}
	asJSON, err := json.Marshal(doc)
	if err != nil {
		return environmentFile{}, fmt.Errorf("the definition file is not a valid Environment: %w", err)
	}
	var f environmentFile
	dec := json.NewDecoder(bytes.NewReader(asJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return environmentFile{}, fmt.Errorf("the definition file is not a valid Environment: %w", err)
	}
	return f, nil
}

func readEnvironmentFile(cmd *cobra.Command, path string) ([]byte, error) {
	r := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the user names the file to apply
		if err != nil {
			return nil, domain.NewError(domain.CodeInvalidRequest, "cannot read the definition file: "+err.Error(), nil)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxEnvironmentFile+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxEnvironmentFile {
		return nil, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("the definition file is larger than %d bytes", maxEnvironmentFile), nil)
	}
	return raw, nil
}

func newEnvironmentApplyCmd(open func(*cobra.Command) (environmentStore, error)) *cobra.Command {
	var file, name, requestID string
	cmd := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Make the definition in a file the next version of an Environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := readEnvironmentFile(cmd, file)
			if err != nil {
				return err
			}
			def, err := decodeEnvironmentFile(raw)
			if err != nil {
				return usageError(err)
			}
			if name == "" {
				name = def.Name
			}
			if name == "" || (def.Name != "" && def.Name != name) {
				return usageError(errors.New("give the Environment a name, once: --name or the name of the file"))
			}
			store, err := open(cmd)
			if err != nil {
				return err
			}
			v, err := store.ApplyEnvironment(cmd.Context(), name, v1.ApplyEnvironmentRequest{RequestID: requestID, Definition: def.EnvironmentDef})
			if nothingToCommit(err) {
				return emitUnchanged(cmd, name, store)
			}
			if err != nil {
				return err
			}
			return emit(cmd, v, func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "%s is at v%d (%s).\n", printable(v.Environment), v.Version, shortDigest(v.Digest))
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&file, "file", "f", "", "definition file; - reads the standard input")
	f.StringVar(&name, "name", "", "name of the Environment (default: the name in the file)")
	f.StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it applies nothing twice")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// nothingToCommit tells the refusal of an apply of the definition of the head.
func nothingToCommit(err error) bool {
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
		return false
	}
	flag, _ := de.Details["nothing_to_commit"].(bool)
	return flag
}

// emitUnchanged is the answer to an apply that changed nothing: the head, and
// "No changes" on the error stream. It is not a failure.
func emitUnchanged(cmd *cobra.Command, name string, store environmentStore) error {
	shown, err := store.ShowEnvironment(cmd.Context(), name)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "No changes against %s v%d.\n", printable(name), shown.Version)
	v := shown.EnvironmentVersion
	v.Definition = nil
	return emit(cmd, v, func(out io.Writer) error {
		_, err := fmt.Fprintf(out, "%s is at v%d (%s).\n", printable(v.Environment), v.Version, shortDigest(v.Digest))
		return err
	})
}

func newEnvironmentListCmd(open func(*cobra.Command) (environmentStore, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Environments and their head version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := open(cmd)
			if err != nil {
				return err
			}
			list, err := store.ListEnvironments(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, v1.ListEnvironmentsResponse{Environments: list}, func(out io.Writer) error {
				w := newTable(out, "NAME\tHEAD\tDIGEST\tCREATED")
				for _, e := range list {
					head := "-"
					if e.Head > 0 {
						head = "v" + strconv.Itoa(e.Head)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", printable(e.Name), head, dash(shortDigest(e.HeadDigest)), e.CreatedAt.Local().Format(time.DateOnly))
				}
				return w.Flush()
			})
		},
	}
}

func newEnvironmentShowCmd(open func(*cobra.Command) (environmentStore, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name[@version]>",
		Short: "Show a version of an Environment: its definition and its versions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := open(cmd)
			if err != nil {
				return err
			}
			shown, err := store.ShowEnvironment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, shown, func(out io.Writer) error { return writeEnvironment(out, shown) })
		},
	}
}

func writeEnvironment(out io.Writer, s v1.ShowEnvironmentResponse) error {
	fmt.Fprintf(out, "Environment  %s v%d\nDigest       %s\nAuthor       %s:%s\nTime         %s\n",
		printable(s.Environment), s.Version, s.Digest, printable(s.Author.Kind), printable(s.Author.ID), s.CreatedAt.Local().Format(time.DateTime))
	if d := s.Definition; d != nil {
		fmt.Fprintf(out, "Provider     %s\nRequires     %s\nPrepare      %s\nDigest in    %s\n",
			printable(d.Provider), dash(printable(strings.Join(d.RequiresCapabilities, ", "))), dash(printable(d.Prepare)), dash(printable(strings.Join(d.DigestInputs, ", "))))
		for _, svc := range d.Services {
			fmt.Fprintf(out, "Service      %s: start %s; stop %s; ready %s\n",
				printable(svc.Name), printable(svc.Start), dash(printable(svc.Stop)), dash(printable(svc.Ready)))
		}
	}
	fmt.Fprintln(out)
	w := newTable(out, "VERSION\tDIGEST\tAUTHOR\tTIME")
	for _, v := range s.Versions {
		fmt.Fprintf(w, "v%d\t%s\t%s\t%s\n", v.Version, shortDigest(v.Digest), printable(v.Author.Kind+":"+v.Author.ID), v.CreatedAt.Local().Format(time.DateTime))
	}
	return w.Flush()
}
