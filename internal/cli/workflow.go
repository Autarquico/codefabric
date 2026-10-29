package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/workflowadopt"
	"github.com/Autarquico/codefabric/internal/domain"
)

// WorkflowComponent is a component a harness offers to adopt.
type WorkflowComponent struct {
	Name string
	Kind string
}

// WorkflowAdopter is what adopt and commit need from the machine: the
// harness adapters and the host whose configuration is read. cmd/cf composes
// it (ADR 0001); the CLI only sees this seam. *workflowadopt.Adopter does the
// Prepare and the Commit.
type WorkflowAdopter interface {
	// Harnesses are the IDs of the harnesses that can be adopted.
	Harnesses() []string
	// Components are the components harness offers, for the checklist.
	Components(ctx context.Context, harness string) ([]WorkflowComponent, error)
	Prepare(ctx context.Context, sel []workflowadopt.Selection) (*workflowadopt.Prepared, error)
	Commit(ctx context.Context, name string, p *workflowadopt.Prepared, parent int, changelog string) (v1.WorkflowVersion, error)
}

// WorkflowAdopterFactory builds the adopter over the Controller store and the
// way the user decides about secrets (nil: headless without --replace-secrets).
type WorkflowAdopterFactory func(store workflowadopt.Store, confirmer workflowadopt.Confirmer) WorkflowAdopter

// workflowStore is the part of *client.Client the workflow commands use.
type workflowStore interface {
	workflowadopt.Store
	ListWorkflows(ctx context.Context) ([]v1.WorkflowSummary, error)
	WorkflowHistory(ctx context.Context, name string) ([]v1.WorkflowVersion, error)
}

// workflowDeps are what the commands are composed with; tests replace them.
type workflowDeps struct {
	open    func(*cobra.Command) (workflowStore, error)
	adopter WorkflowAdopterFactory
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f)
}

func workflowDepsOf(opts Options) workflowDeps {
	open := opts.workflowStore
	if open == nil {
		open = func(cmd *cobra.Command) (workflowStore, error) { return openClient(cmd) }
	}
	return workflowDeps{open: open, adopter: opts.Workflows}
}

const (
	flagHarness        = "harness"
	flagComponent      = "component"
	flagLocal          = "local"
	flagReplaceSecrets = "replace-secrets"
	flagYes            = "yes"
	flagChangelog      = "changelog"
	flagRebase         = "rebase"
)

func newWorkflowCmd(deps workflowDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "workflow", Short: "Adopt, version and inspect Workflows (native harness configuration)"}
	cmd.AddCommand(newWorkflowListCmd(deps), newWorkflowShowCmd(deps), newWorkflowHistoryCmd(deps),
		newWorkflowAdoptCmd(deps), newWorkflowCommitCmd(deps))
	return cmd
}

func newWorkflowListCmd(deps workflowDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Workflows and their head version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := deps.open(cmd)
			if err != nil {
				return err
			}
			list, err := store.ListWorkflows(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, v1.ListWorkflowsResponse{Workflows: list}, func(out io.Writer) error {
				w := newTable(out, "NAME\tHEAD\tDIGEST\tCREATED")
				for _, wf := range list {
					head := "-"
					if wf.Head > 0 {
						head = "v" + strconv.Itoa(wf.Head)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", printable(wf.Name), head, dash(shortDigest(wf.HeadDigest)), wf.CreatedAt.Local().Format(time.DateOnly))
				}
				return w.Flush()
			})
		},
	}
}

func newWorkflowShowCmd(deps workflowDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name[@version]>",
		Short: "Show a version of a Workflow: its metadata and manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := deps.open(cmd)
			if err != nil {
				return err
			}
			v, err := store.ShowWorkflow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, v, func(out io.Writer) error {
				if err := writeVersion(out, v); err != nil {
					return err
				}
				if v.Manifest == nil {
					return nil
				}
				fmt.Fprintln(out)
				return writeManifest(out, *v.Manifest)
			})
		},
	}
}

func newWorkflowHistoryCmd(deps workflowDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "history <name>",
		Short: "List the versions of a Workflow, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := deps.open(cmd)
			if err != nil {
				return err
			}
			versions, err := store.WorkflowHistory(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, v1.WorkflowHistoryResponse{Versions: versions}, func(out io.Writer) error {
				w := newTable(out, "VERSION\tAUTHOR\tORIGIN\tTIME\tCHANGED\tCHANGELOG")
				for _, v := range versions {
					fmt.Fprintf(w, "v%d\t%s\t%s\t%s\t%s\t%s\n", v.Version, printable(authorOf(v)), printable(v.Origin),
						v.CreatedAt.Local().Format(time.DateTime), dash(changedLine(v.Changed)), dash(printable(v.Changelog)))
				}
				return w.Flush()
			})
		},
	}
}

func writeVersion(out io.Writer, v v1.WorkflowVersion) error {
	_, err := fmt.Fprintf(out, "Workflow   %s v%d\nDigest     %s\nAuthor     %s (%s)\nTime       %s\nChangelog  %s\nChanged    %s\n",
		printable(v.Workflow), v.Version, v.Digest, printable(authorOf(v)), printable(v.Origin),
		v.CreatedAt.Local().Format(time.DateTime), dash(printable(v.Changelog)), dash(changedLine(v.Changed)))
	return err
}

func writeManifest(out io.Writer, m domain.Manifest) error {
	w := newTable(out, "HARNESS\tCOMPONENT\tKIND\tOWNERSHIP\tFILES\tTREE DIGEST")
	for _, c := range m.Components {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", printable(c.Harness), printable(c.Name), printable(c.Kind), c.Ownership, len(c.Files), shortDigest(c.TreeDigest))
	}
	return w.Flush()
}

func authorOf(v v1.WorkflowVersion) string { return v.Author.Kind + ":" + v.Author.ID }

func changedLine(changed []v1.ChangedComponent) string {
	parts := make([]string, len(changed))
	for i, c := range changed {
		parts[i] = fmt.Sprintf("%s %s/%s", c.Change, c.Harness, c.Name)
	}
	return printable(strings.Join(parts, ", "))
}

// shortDigest keeps the algorithm and the first 12 hex digits.
func shortDigest(d string) string {
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) > i+1+shortCommit {
		return d[:i+1+shortCommit]
	}
	return d
}

// adoptFlags are the choices adopt and commit share; each one is also asked
// on a terminal when it is not given.
type adoptFlags struct {
	harnesses, components, local []string
	replaceSecrets, yes          bool
	changelog                    string
	rebase                       bool
}

func (f *adoptFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.harnesses, flagHarness, nil, "harness to include (repeatable); asked on a terminal when omitted")
	fl.StringSliceVar(&f.components, flagComponent, nil, "component to include, by name or harness/name (repeatable); default: every adoptable one")
	fl.StringSliceVar(&f.local, flagLocal, nil, "component to mark as local instead of managed (repeatable)")
	fl.BoolVar(&f.replaceSecrets, flagReplaceSecrets, false, "accept every secret ref and replace the secret-looking values by markers; without it a headless run aborts if there is any")
	fl.BoolVar(&f.yes, flagYes, false, "do not ask for the final confirmation")
	fl.StringVar(&f.changelog, flagChangelog, "", "what this version changes")
}

func newWorkflowAdoptCmd(deps workflowDeps) *cobra.Command {
	var f adoptFlags
	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Create a Workflow from the native configuration of this machine",
		Long: "Snapshots the native configuration of the chosen harnesses on this machine and stores it as version 1 of a new Workflow.\n" +
			"On a terminal it asks for harnesses, components, ownership and every secret ref before anything leaves the machine; " +
			"headless it needs --harness, --yes and, if the configuration has secret refs, --replace-secrets. Secret values are never sent.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdopt(cmd, deps, args[0], f)
		},
	}
	f.bind(cmd)
	return cmd
}

func newWorkflowCommitCmd(deps workflowDeps) *cobra.Command {
	var f adoptFlags
	cmd := &cobra.Command{
		Use:   "commit <name>",
		Short: "Commit the current native configuration as a new version of a Workflow",
		Long: "Snapshots this machine again (the components of the head unless you name others), shows the diff against the head and commits it on top.\n" +
			"If the head moved meanwhile the commit fails with CF_WORKFLOW_CONFLICT (exit 4); with --rebase it re-reads the head, " +
			"snapshots again, shows the new diff and asks again (headless: --yes). A second conflict is final. Nothing is ever merged.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCommit(cmd, deps, args[0], f)
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&f.rebase, flagRebase, false, "on a conflict, re-read the head, snapshot again and commit on top of it after confirming the diff")
	return cmd
}

// session is one adopt or commit in progress: the adopter, and how it talks to
// the user. ui is nil when headless.
type session struct {
	cmd     *cobra.Command
	adopter WorkflowAdopter
	store   workflowStore
	ui      *prompter
	flags   adoptFlags
	// confirmer is how the user decides about secrets and diffs.
	confirmer cliConfirmer
}

func startSession(cmd *cobra.Command, deps workflowDeps, f adoptFlags) (*session, error) {
	if deps.adopter == nil {
		return nil, domain.NewError(domain.CodeNotImplemented, "this binary has no harness adapters to adopt from", nil)
	}
	s := &session{cmd: cmd, flags: f}
	if isTerminal(cmd.InOrStdin()) && isTerminalWriter(cmd.OutOrStdout()) {
		s.ui = &prompter{in: bufio.NewReader(cmd.InOrStdin()), out: cmd.ErrOrStderr()}
	} else if !f.yes {
		return nil, usageError(errors.New("without a terminal there is nobody to confirm: pass --" + flagYes))
	}
	store, err := deps.open(cmd)
	if err != nil {
		return nil, err
	}
	s.store = store
	s.confirmer = cliConfirmer{ui: s.ui, out: cmd.ErrOrStderr(), yes: f.yes, replaceSecrets: f.replaceSecrets}
	s.adopter = deps.adopter(store, s.confirmer)
	return s, nil
}

func runAdopt(cmd *cobra.Command, deps workflowDeps, name string, f adoptFlags) error {
	s, err := startSession(cmd, deps, f)
	if err != nil {
		return err
	}
	sel, err := s.chooseSelection(cmd.Context())
	if err != nil {
		return err
	}
	p, err := s.adopter.Prepare(cmd.Context(), sel)
	if err != nil {
		return adoptError(err)
	}
	if err := s.chooseOwnership(&p.Manifest); err != nil {
		return err
	}
	if err := s.confirmAdopt(name, p.Manifest.Components); err != nil {
		return err
	}
	v, err := s.adopter.Commit(cmd.Context(), name, p, 0, f.changelog)
	if err != nil {
		return adoptError(err)
	}
	return emitCommitted(cmd, v)
}

func runCommit(cmd *cobra.Command, deps workflowDeps, name string, f adoptFlags) error {
	s, err := startSession(cmd, deps, f)
	if err != nil {
		return err
	}
	if len(f.harnesses) == 0 && len(f.components) > 0 {
		return usageError(errors.New("--" + flagComponent + " needs --" + flagHarness))
	}
	var sel []workflowadopt.Selection
	if len(f.harnesses) > 0 {
		sel = selectionOf(f.harnesses, f.components)
	}
	got, err := workflowadopt.CommitOnHead(cmd.Context(), s.adopter, s.store, name, workflowadopt.CommitOptions{
		Selection: sel, Changelog: f.changelog, Rebase: f.rebase, Confirmer: s.confirmer,
		Adjust: func(m *domain.Manifest) error { return s.chooseOwnership(m) },
	})
	if err != nil {
		return adoptError(err)
	}
	if got.Unchanged {
		fmt.Fprintf(cmd.ErrOrStderr(), "No changes against %s v%d.\n", name, got.Version.Version)
	}
	return emitCommitted(cmd, got.Version)
}

func selectionOf(harnesses, components []string) []workflowadopt.Selection {
	out := make([]workflowadopt.Selection, len(harnesses))
	for i, h := range harnesses {
		out[i] = workflowadopt.Selection{Harness: h, Components: components}
	}
	return out
}

// chooseSelection is the harnesses and components to adopt: the flags, or the
// checklist on a terminal.
func (s *session) chooseSelection(ctx context.Context) ([]workflowadopt.Selection, error) {
	harnesses := s.flags.harnesses
	if len(harnesses) == 0 {
		if s.ui == nil {
			return nil, usageError(errors.New("without a terminal, say which harness to adopt with --" + flagHarness))
		}
		available := s.adopter.Harnesses()
		if len(available) == 0 {
			return nil, domain.NewError(domain.CodeNotImplemented, "no harness can be adopted on this machine", nil)
		}
		picked, err := s.ui.pick("Harnesses to include", available, nil)
		if err != nil {
			return nil, err
		}
		for _, i := range picked {
			harnesses = append(harnesses, available[i])
		}
	}
	if len(harnesses) == 0 {
		return nil, usageError(errors.New("no harness selected"))
	}
	if len(s.flags.components) > 0 || s.ui == nil {
		return selectionOf(harnesses, s.flags.components), nil
	}
	var out []workflowadopt.Selection
	for _, h := range harnesses {
		offered, err := s.adopter.Components(ctx, h)
		if err != nil {
			return nil, err
		}
		labels := make([]string, len(offered))
		for i, c := range offered {
			labels[i] = fmt.Sprintf("%s (%s)", c.Name, c.Kind)
		}
		picked, err := s.ui.pick("Components of "+h+" to include", labels, nil)
		if err != nil {
			return nil, err
		}
		sel := workflowadopt.Selection{Harness: h}
		for _, i := range picked {
			sel.Components = append(sel.Components, offered[i].Name)
		}
		if len(sel.Components) > 0 {
			out = append(out, sel)
		}
	}
	return out, nil
}

// chooseOwnership marks local the components the flags name, or the ones the
// user picks on a terminal.
func (s *session) chooseOwnership(m *domain.Manifest) error {
	names := s.flags.local
	if len(names) == 0 && s.ui != nil {
		labels := make([]string, len(m.Components))
		for i, c := range m.Components {
			labels[i] = fmt.Sprintf("%s/%s (now %s)", c.Harness, c.Name, c.Ownership)
		}
		picked, err := s.ui.pick("Components to keep local (the rest stay as listed)", labels, []int{})
		if err != nil {
			return err
		}
		for _, i := range picked {
			names = append(names, m.Components[i].Harness+"/"+m.Components[i].Name)
		}
	}
	return markLocal(m, names)
}

// markLocal sets ownership local on the components named, by name or
// harness/name; a name that matches nothing is a usage error.
func markLocal(m *domain.Manifest, names []string) error {
	for _, n := range names {
		found := false
		for i := range m.Components {
			c := &m.Components[i]
			if c.Name == n || c.Harness+"/"+c.Name == n {
				c.Ownership, found = domain.OwnershipLocal, true
			}
		}
		if !found {
			return usageError(fmt.Errorf("--%s %q: no such component in this snapshot", flagLocal, n))
		}
	}
	return nil
}

// confirmAdopt shows what is about to be adopted and asks, unless --yes.
func (s *session) confirmAdopt(name string, components []domain.ManifestComponent) error {
	errOut := s.cmd.ErrOrStderr()
	for _, c := range components {
		fmt.Fprintf(errOut, "  %s/%s  %s  %s  %d file(s)\n", printable(c.Harness), printable(c.Name), printable(c.Kind), c.Ownership, len(c.Files))
	}
	ok, err := s.confirmer.ask(fmt.Sprintf("Adopt %d component(s) as %s v1?", len(components), name))
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewError(domain.CodeConfirmationRequired, "not confirmed; nothing was committed", nil)
	}
	return nil
}

func emitCommitted(cmd *cobra.Command, v v1.WorkflowVersion) error {
	return emit(cmd, v, func(out io.Writer) error {
		_, err := fmt.Fprintf(out, "%s is at v%d (%s).\n", printable(v.Workflow), v.Version, shortDigest(v.Digest))
		return err
	})
}

// adoptError gives the refusals of the adopt their place in the §40 codes:
// they are about the request (what was asked cannot be adopted safely).
func adoptError(err error) error {
	for _, refusal := range []error{workflowadopt.ErrSecretsUnconfirmed, workflowadopt.ErrNotReplaceable, workflowadopt.ErrAuthInSnapshot} {
		if errors.Is(err, refusal) {
			return domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
		}
	}
	return err
}

// prompter is the thin terminal part: numbered lists and yes/no, on a reader
// and a writer.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

var errNoAnswer = domain.NewError(domain.CodeConfirmationRequired, "no answer: input ended", nil)

func (p *prompter) line() (string, error) {
	s, err := p.in.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		return "", errNoAnswer
	}
	return strings.TrimSpace(s), nil
}

// pick lists items and returns the indexes chosen: numbers separated by
// commas, "all", "none", or Enter for def (nil def is all).
func (p *prompter) pick(title string, items []string, def []int) ([]int, error) {
	if def == nil {
		def = make([]int, len(items))
		for i := range items {
			def[i] = i
		}
	}
	for {
		fmt.Fprintln(p.out, title+":")
		for i, it := range items {
			mark := " "
			if slices.Contains(def, i) {
				mark = "x"
			}
			fmt.Fprintf(p.out, "  [%s] %d. %s\n", mark, i+1, printable(it))
		}
		fmt.Fprint(p.out, "Numbers separated by commas, all, none; Enter keeps the marked: ")
		answer, err := p.line()
		if err != nil {
			return nil, err
		}
		if picked, ok := parsePick(answer, len(items), def); ok {
			return picked, nil
		}
		fmt.Fprintln(p.out, "Not understood.")
	}
}

func parsePick(answer string, n int, def []int) ([]int, bool) {
	switch strings.ToLower(answer) {
	case "":
		return def, true
	case "none":
		return []int{}, true
	case "all":
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out, true
	}
	var out []int
	for _, part := range strings.Split(answer, ",") {
		i, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || i < 1 || i > n {
			return nil, false
		}
		if !slices.Contains(out, i-1) {
			out = append(out, i-1)
		}
	}
	slices.Sort(out)
	return out, true
}

// confirm asks a yes/no question whose default is no.
func (p *prompter) confirm(question string) (bool, error) {
	fmt.Fprint(p.out, question+" [y/N] ")
	answer, err := p.line()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// cliConfirmer is how the user decides: on a terminal by asking, headless by
// the flags. --yes answers the questions about a diff; --replace-secrets
// answers the one about secrets.
type cliConfirmer struct {
	ui             *prompter // nil when headless
	out            io.Writer
	yes            bool
	replaceSecrets bool
}

// ask is a yes/no question: true without asking when --yes is set or nobody is
// there to ask.
func (c cliConfirmer) ask(question string) (bool, error) {
	if c.yes || c.ui == nil {
		return true, nil
	}
	return c.ui.confirm(question)
}

// ConfirmChanges shows the diff and asks once.
func (c cliConfirmer) ConfirmChanges(_ context.Context, r workflowadopt.Review) (bool, error) {
	if r.Attempt > 0 {
		fmt.Fprintf(c.out, "The head of %s moved; rebased on v%d.\n", printable(r.Name), r.Head)
	}
	for _, ch := range r.Diff {
		fmt.Fprintf(c.out, "  %-8s %s/%s\n", ch.Change, printable(ch.Harness), printable(ch.Name))
	}
	return c.ask(fmt.Sprintf("Commit these changes as %s v%d?", r.Name, r.Head+1))
}

// ConfirmSecrets lists every secret ref of the snapshot (the adapter's and the
// scanner's), with redacted previews, and asks once for all of them.
func (c cliConfirmer) ConfirmSecrets(_ context.Context, found []workflowadopt.HarnessSecrets) (bool, error) {
	if c.replaceSecrets {
		return true, nil
	}
	if c.ui == nil {
		return false, fmt.Errorf("%w: the snapshot has secret refs and this adopt is headless without --%s", workflowadopt.ErrSecretsUnconfirmed, flagReplaceSecrets)
	}
	fmt.Fprintln(c.ui.out, "Secret refs in this configuration (values are never sent):")
	for _, f := range found {
		for _, s := range f.Substituted {
			fmt.Fprintf(c.ui.out, "  %s/%s %s: ref %s (already a marker)\n", printable(f.Harness), printable(s.Component), printable(s.File), printable(s.Ref))
		}
		for _, h := range f.Hits {
			fmt.Fprintf(c.ui.out, "  %s/%s %s:%d: %s %s -> ref %s\n", printable(f.Harness), printable(h.Component), printable(h.File), h.Line, printable(h.Kind), printable(h.Preview), printable(h.Ref))
		}
	}
	return c.ui.confirm("Replace the values by their markers and continue?")
}
