package actions_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// notActions are the leaves of the cf tree that have no action, and why. A new
// command is either an action or here: the parity test fails until it is one.
var notActions = map[string]string{
	// Reads: the views read through the SDK, which is what these commands call.
	"cf client list": "read", "cf config paths": "read", "cf config show": "read", "cf doctor": "read",
	"cf environment list": "read", "cf environment show": "read", "cf events": "read", "cf logs": "read",
	"cf project explain": "read", "cf project list": "read", "cf project secrets list": "read", "cf project show": "read",
	"cf ps": "read", "cf notify watch": "long-running foreground watcher", "cf notify test": "notifies this machine, not the cluster", "cf run explain": "read", "cf run list": "read", "cf run show": "read", "cf status": "read",
	"cf task list": "read", "cf task show": "read", "cf version": "read", "cf worker list": "read", "cf worker show": "read",
	"cf workflow diff": "read", "cf workflow explain": "read", "cf workflow history": "read", "cf workflow list": "read",
	"cf workflow show": "read", "cf workflow status": "read",
	// This machine's identity and setup, not something the TUI does to the cluster.
	"cf connect": "enrols this machine", "cf client remove": "revokes a client identity", "cf client renew": "renews this machine's credentials",
	// Same command as an action that exists.
	"cf do": "shortcut of task.new, which needs the working directory of a shell",
}

func leaves(root *cobra.Command) map[string]*cobra.Command {
	out := map[string]*cobra.Command{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.HasSubCommands() {
			out[c.CommandPath()] = c
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// flagValueRe matches the placeholder of a flag's value in a Use line: "-f <file>".
var flagValueRe = regexp.MustCompile(`--?\w[\w-]* <[^>]*>`)

// checkInputs requires the descriptor to be able to satisfy its command: every
// flag it declares exists, every flag the command requires is a required input,
// and the positional arguments match the ones the command takes.
func checkInputs(t *testing.T, d actions.Descriptor, path string, cmd *cobra.Command) {
	t.Helper()
	declared := map[string]actions.Arg{}
	positional := 0
	if d.Target != actions.None {
		positional++
	}
	for _, a := range d.Args {
		declared[a.Name] = a
		switch {
		case !a.Flag:
			positional++
		case cmd.Flags().Lookup(a.Name) == nil:
			t.Errorf("%s: command %q has no flag --%s", d.ID, path, a.Name)
		}
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_, cobraRequires := f.Annotations[cobra.BashCompOneRequiredFlag]
		_, cfRequires := f.Annotations[cli.RequiredFlagAnnotation]
		if !cobraRequires && !cfRequires {
			return
		}
		if a, ok := declared[f.Name]; !ok || !a.Flag || !a.Required {
			t.Errorf("%s: command %q requires --%s and the action does not ask for it", d.ID, path, f.Name)
		}
	})
	if want := strings.Count(flagValueRe.ReplaceAllString(cmd.Use, ""), "<"); want != positional {
		t.Errorf("%s: command %q takes %d positional arguments (%q), the action gives %d", d.ID, path, want, cmd.Use, positional)
	}
}

func TestScenario_TuiViewsActions_ParidadCLI(t *testing.T) {
	root := cli.CommandTree(cli.Options{})
	tree := leaves(root)
	covered := map[string]string{}
	ids := map[string]bool{}

	for _, d := range actions.All() {
		if ids[d.ID] {
			t.Errorf("%s is registered twice", d.ID)
		}
		ids[d.ID] = true
		path := strings.Join(append([]string{"cf"}, d.Path...), " ")
		cmd, ok := tree[path]
		if !ok {
			t.Errorf("%s: no command %q in the cobra tree", d.ID, path)
			continue
		}
		if other, dup := covered[path]; dup {
			t.Errorf("%s and %s run the same command %q", d.ID, other, path)
		}
		covered[path] = d.ID
		checkInputs(t, d, path, cmd)
		if d.Title == "" {
			t.Errorf("%s has no title", d.ID)
		}
	}

	for path := range tree {
		_, isAction := covered[path]
		_, excluded := notActions[path]
		switch {
		case isAction && excluded:
			t.Errorf("%q is an action and also excluded", path)
		case !isAction && !excluded:
			t.Errorf("%q is a command with no action and no reason to have none", path)
		}
	}
	for path := range notActions {
		if _, ok := tree[path]; !ok {
			t.Errorf("%q is excluded but is not a command", path)
		}
	}
}

func TestScenario_TuiViewsActions_NoDaemonCommandsInTheTree(t *testing.T) {
	// The binary composes the daemon commands in; the registry runs the CLI tree without them.
	tree := leaves(cli.CommandTree(cli.Options{}))
	for _, daemon := range []string{"cf controller init", "cf controller run", "cf setup", "cf install-service", "cf worker run", "cf worker join"} {
		if _, ok := tree[daemon]; ok {
			t.Errorf("the tree the registry runs holds the daemon command %q", daemon)
		}
	}
}

// domainCodes reads every Code the domain declares from its source, so a new one
// is checked without anybody remembering to list it.
func domainCodes(t *testing.T) []domain.Code {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../../domain/errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var codes []domain.Code
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || spec.Type == nil || fmt.Sprint(spec.Type) != "Code" {
			return true
		}
		for _, v := range spec.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				value, _ := strconv.Unquote(lit.Value)
				codes = append(codes, domain.Code(value))
			}
		}
		return true
	})
	if len(codes) < 25 {
		t.Fatalf("read only %d codes from the domain", len(codes))
	}
	return codes
}

func TestScenario_TuiViewsActions_EveryCodeHasAHint(t *testing.T) {
	for _, code := range domainCodes(t) {
		if actions.HintFor(code, nil) == "" {
			t.Errorf("%s has no recoverable hint", code)
		}
	}
	if actions.HintFor(domain.CodeRunNotAttachable, map[string]any{"reason": "no_ssh_target"}) == actions.HintFor(domain.CodeRunNotAttachable, nil) {
		t.Error("no_ssh_target gets the generic attach hint")
	}
}

func TestScenario_TuiViewsActions_Errores(t *testing.T) {
	t.Setenv("CF_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := actions.Runner{}
	ctx := t.Context()

	cases := map[string]struct {
		id   string
		in   actions.Input
		code domain.Code
		exit int
	}{
		"no target":         {"task.cancel", actions.Input{}, domain.CodeInvalidRequest, 2},
		"missing arg":       {"task.move", actions.Input{Target: "t"}, domain.CodeInvalidRequest, 2},
		"unknown arg":       {"task.cancel", actions.Input{Target: "t", Values: map[string]string{"nope": "x"}}, domain.CodeInvalidRequest, 2},
		"unknown action":    {"task.explode", actions.Input{}, domain.CodeInvalidRequest, 2},
		"bad read-only":     {"run.attach", actions.Input{Target: "r", Values: map[string]string{"read-only": "maybe"}}, domain.CodeInvalidRequest, 2},
		"terminal action":   {"run.attach", actions.Input{Target: "r"}, domain.CodeInvalidRequest, 2},
		"workflow no input": {"workflow.apply", actions.Input{Target: "w"}, domain.CodeInvalidRequest, 2},
		"command rejects":   {"task.new", actions.Input{Values: map[string]string{"prompt": "x", "completion": "bogus"}}, "", 0},
		"controller absent": {"task.cancel", actions.Input{Target: "auth-race"}, "", 0},
		// A target that looks like a flag stays data: it fails for the missing credentials, it is not help.
		"flag-like target": {"task.cancel", actions.Input{Target: "--help"}, domain.CodeUnauthenticated, 6},
	}
	for name, c := range cases {
		res := runner.Run(ctx, c.id, c.in)
		f := res.Failure
		if f == nil {
			t.Errorf("%s: it worked: %s", name, res.Output)
			continue
		}
		if f.Code == "" || f.Message == "" || f.ExitCode == 0 {
			t.Errorf("%s: incomplete failure %+v", name, f)
		}
		if c.code != "" && (f.Code != c.code || f.ExitCode != c.exit) {
			t.Errorf("%s: %s exit %d, want %s exit %d", name, f.Code, f.ExitCode, c.code, c.exit)
		}
		if f.ExitCode != domain.ExitCode(f.Code) {
			t.Errorf("%s: %s exits %d, the CLI exits %d", name, f.Code, f.ExitCode, domain.ExitCode(f.Code))
		}
		if f.Hint == "" {
			t.Errorf("%s: %s has no recoverable hint", name, f.Code)
		}
	}
}
