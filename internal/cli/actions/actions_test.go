package actions_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
	"cf ps": "read", "cf run explain": "read", "cf run list": "read", "cf run show": "read", "cf status": "read",
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
		for _, a := range d.Args {
			if a.Flag && cmd.Flags().Lookup(a.Name) == nil {
				t.Errorf("%s: command %q has no flag --%s", d.ID, path, a.Name)
			}
		}
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
	for _, d := range actions.All() {
		switch d.Path[0] {
		case "controller", "setup", "install-service":
			t.Errorf("%s runs a daemon or setup command", d.ID)
		}
		if d.Path[0] == "worker" && slices.Contains([]string{"run", "join"}, d.Path[1]) {
			t.Errorf("%s runs a Worker daemon command", d.ID)
		}
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
		"unknown action":    {"task.explode", actions.Input{}, domain.CodeNotFound, 3},
		"terminal action":   {"run.attach", actions.Input{Target: "r"}, domain.CodeInvalidRequest, 2},
		"command rejects":   {"task.new", actions.Input{Values: map[string]string{"prompt": "x", "completion": "bogus"}}, "", 0},
		"controller absent": {"task.cancel", actions.Input{Target: "auth-race"}, "", 0},
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
		if f.Hint == "" && f.Code != domain.CodeNotFound {
			t.Errorf("%s: %s has no recoverable hint", name, f.Code)
		}
	}
}
