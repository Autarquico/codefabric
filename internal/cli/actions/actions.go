// Package actions is the registry of what a person can do to the system from a
// screen: the TUI and its command palette list and run these, and each one is a
// command of the cf tree (§19.4, §38).
//
// An action does not reimplement its command. Running it builds a fresh cf
// command tree and executes the command's path with the arguments, so
// validation, the call to the Controller and the mapping of errors to exit
// codes are the command's own, and no flag or context state survives from one
// run to the next.
package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Kind is what an action acts on.
type Kind string

// The kinds of target. None is an action that needs no existing thing.
const (
	None     Kind = ""
	Task     Kind = "task"
	Run      Kind = "run"
	Worker   Kind = "worker"
	Workflow Kind = "workflow"
	Project  Kind = "project"
)

// Arg is an input of an action besides its target: a positional argument, or a
// flag of the command when Flag is set.
type Arg struct {
	Name     string
	Flag     bool
	Required bool
}

// Descriptor says what an action is and which command runs it.
type Descriptor struct {
	ID    string
	Title string
	// Target is the kind of the thing the action acts on; its reference is the
	// first positional argument of the command.
	Target Kind
	// Args are the other inputs; positional ones follow the target in this order.
	Args []Arg
	// Path is the command in the cf tree, without the root: ["task", "cancel"].
	Path []string
	// Terminal marks an action that takes over the terminal (attach): it is
	// prepared with Runner.Attach, and the caller runs the process.
	Terminal bool
}

func flags(names ...string) []Arg {
	args := make([]Arg, len(names))
	for i, n := range names {
		args[i] = Arg{Name: n, Flag: true}
	}
	return args
}

func with(args []Arg, more ...Arg) []Arg { return append(more, args...) }

var projectFlag = flags("project")

var descriptors = []Descriptor{
	{
		ID: "task.new", Title: "New task", Path: []string{"task", "new"},
		Args: with(flags("project", "name", "ref", "harness", "model", "completion"), Arg{Name: "prompt", Required: true}),
	},
	{ID: "task.set", Title: "Edit task", Target: Task, Path: []string{"task", "set"}, Args: flags("name", "completion", "project")},
	{ID: "task.cancel", Title: "Cancel task", Target: Task, Path: []string{"task", "cancel"}, Args: projectFlag},
	{ID: "task.complete", Title: "Complete task", Target: Task, Path: []string{"task", "complete"}, Args: projectFlag},
	{ID: "task.archive", Title: "Archive task", Target: Task, Path: []string{"task", "archive"}, Args: projectFlag},
	{ID: "task.defer", Title: "Defer task", Target: Task, Path: []string{"task", "defer"}, Args: projectFlag},
	{ID: "task.resume", Title: "Resume task", Target: Task, Path: []string{"task", "resume"}, Args: projectFlag},
	{
		ID: "task.move", Title: "Move task to another worker", Target: Task, Path: []string{"task", "move"},
		Args: with(projectFlag, Arg{Name: "worker", Required: true}),
	},
	{ID: "task.timeline", Title: "Task timeline", Target: Task, Path: []string{"task", "timeline"}, Args: flags("after", "limit", "project")},

	{ID: "run.attach", Title: "Attach to run", Target: Run, Path: []string{"attach"}, Terminal: true, Args: flags("read-only", "ssh-target", "project")},
	{ID: "run.fork", Title: "Fork run", Target: Run, Path: []string{"run", "fork"}},
	{ID: "run.promote", Title: "Promote run", Target: Run, Path: []string{"run", "promote"}},
	{ID: "run.publish", Title: "Publish run", Target: Run, Path: []string{"run", "publish"}},

	{ID: "worker.drain", Title: "Drain worker", Target: Worker, Path: []string{"worker", "drain"}},
	{ID: "worker.undrain", Title: "Undrain worker", Target: Worker, Path: []string{"worker", "undrain"}},
	{ID: "worker.remove", Title: "Remove worker", Target: Worker, Path: []string{"worker", "remove"}},

	{ID: "workflow.adopt", Title: "Adopt workflow", Target: Workflow, Path: []string{"workflow", "adopt"}},
	{ID: "workflow.commit", Title: "Commit workflow", Target: Workflow, Path: []string{"workflow", "commit"}},
	{ID: "workflow.apply", Title: "Apply workflow", Target: Workflow, Path: []string{"workflow", "apply"}, Args: flags("dry-run", "preview")},
	{ID: "workflow.rollback", Title: "Roll back workflow", Path: []string{"workflow", "rollback"}},
	{ID: "workflow.sync", Title: "Sync workflow", Target: Workflow, Path: []string{"workflow", "sync"}},

	{
		ID: "project.create", Title: "New project", Path: []string{"project", "create"},
		Args: with(flags("repo", "default-branch"), Arg{Name: "name", Required: true}),
	},
	{
		ID: "project.set", Title: "Edit project", Target: Project, Path: []string{"project", "set"},
		Args: flags("default-branch", "workflow", "environment", "mode", "completion"),
	},
	{
		ID: "project.secrets.allow", Title: "Allow project secret", Target: Project, Path: []string{"project", "secrets", "allow"},
		Args: []Arg{{Name: "ref", Required: true}},
	},
	{
		ID: "project.secrets.deny", Title: "Deny project secret", Target: Project, Path: []string{"project", "secrets", "deny"},
		Args: []Arg{{Name: "ref", Required: true}},
	},
	{
		ID: "environment.apply", Title: "Apply environment", Path: []string{"environment", "apply"},
		Args: []Arg{{Name: "file", Flag: true, Required: true}, {Name: "name", Flag: true}},
	},
}

// All lists the actions, in the order the palette shows them. The slices of the
// result are the caller's.
func All() []Descriptor {
	out := make([]Descriptor, len(descriptors))
	for i, d := range descriptors {
		d.Path = append([]string(nil), d.Path...)
		d.Args = append([]Arg(nil), d.Args...)
		out[i] = d
	}
	return out
}

// Lookup finds an action by ID.
func Lookup(id string) (Descriptor, bool) {
	for _, d := range All() {
		if d.ID == id {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Input is what the person gave an action. Values are keyed by Arg name.
type Input struct {
	Target string
	Values map[string]string
	// Controller is the host:port of this call, as --controller; empty is the stored one.
	Controller string
}

// Result is what a run of a command left: the JSON it wrote or why it failed.
type Result struct {
	// Output is what the command wrote to stdout, as JSON, when it worked.
	Output  []byte
	Failure *Failure
}

// Failure is an error ready to show: its code, what happened, and what can be
// done about it. ExitCode is the one `cf` would exit with.
type Failure struct {
	Code     domain.Code
	Message  string
	Hint     string
	ExitCode int
}

func (f *Failure) Error() string { return string(f.Code) + ": " + f.Message }

// Runner runs actions with the composition of the binary (the Terminal and the
// adopters only cmd/cf can build).
type Runner struct{ Options cli.Options }

// Run executes the command of the action in a fresh command tree. An action that
// takes over the terminal is refused: it goes through Attach.
func (r Runner) Run(ctx context.Context, id string, in Input) Result {
	d, ok := Lookup(id)
	switch {
	case !ok:
		return refused(domain.CodeNotFound, "there is no action "+id)
	case d.Terminal:
		return refused(domain.CodeInvalidRequest, d.Title+" takes over the terminal: prepare it with Attach")
	}
	argv, err := d.argv(in)
	if err != nil {
		return Result{Failure: asFailure(err)}
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunContextWith(ctx, r.Options, argv, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		return Result{Output: stdout.Bytes()}
	}
	return Result{Failure: failureOf(code, stdout.Bytes(), stderr.String())}
}

// Attach prepares an action that takes over the terminal, with the checks of
// the command, and returns the session: its Cmd for the caller to run (the TUI,
// with tea.ExecProcess) and Finish to turn how it ended into the command's answer.
func (r Runner) Attach(ctx context.Context, id string, in Input) (*cli.AttachSession, *Failure) {
	d, ok := Lookup(id)
	if !ok || !d.Terminal {
		return nil, refused(domain.CodeInvalidRequest, id+" is not an action that takes over the terminal").Failure
	}
	if in.Target == "" {
		return nil, asFailure(domain.NewError(domain.CodeInvalidRequest, d.Title+" needs its "+string(d.Target), nil))
	}
	session, err := cli.PrepareAttach(ctx, r.Options.Terminal, cli.AttachParams{
		Ref: in.Target, Project: in.Values["project"], SSHTarget: in.Values["ssh-target"],
		ReadOnly: in.Values["read-only"] == "true", Controller: in.Controller,
	})
	if err != nil {
		return nil, asFailure(err)
	}
	return session, nil
}

// argv is the command line of the action. Flags come first and every positional
// value after `--`, so nothing the person typed can be read as a flag.
func (d Descriptor) argv(in Input) ([]string, error) {
	invalid := func(msg string) error { return domain.NewError(domain.CodeInvalidRequest, msg, nil) }
	argv := append([]string(nil), d.Path...)
	argv = append(argv, "--json")
	if in.Controller != "" {
		argv = append(argv, "--controller="+in.Controller)
	}
	var positional []string
	if d.Target != None {
		if in.Target == "" {
			return nil, invalid(d.Title + " needs its " + string(d.Target))
		}
		positional = append(positional, in.Target)
	}
	known := map[string]bool{}
	for _, a := range d.Args {
		known[a.Name] = true
		value := in.Values[a.Name]
		switch {
		case value == "" && a.Required:
			return nil, invalid(d.Title + " needs " + a.Name)
		case value == "":
		case a.Flag:
			argv = append(argv, "--"+a.Name+"="+value)
		default:
			positional = append(positional, value)
		}
	}
	for name := range in.Values {
		if !known[name] {
			return nil, invalid(d.Title + " takes no " + name)
		}
	}
	if len(positional) > 0 {
		argv = append(argv, "--")
		argv = append(argv, positional...)
	}
	return argv, nil
}

// failureOf reads the §40 envelope the command wrote to stdout under --json.
func failureOf(exit int, stdout []byte, stderr string) *Failure {
	var env v1.ErrorEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil || env.Error.Code == "" {
		return &Failure{Code: domain.CodeInternal, Message: strings.TrimSpace(stderr), Hint: hintFor(domain.CodeInternal, nil), ExitCode: exit}
	}
	e := env.Error
	return &Failure{Code: e.Code, Message: e.Message, Hint: hintFor(e.Code, e.Details), ExitCode: exit}
}

// asFailure presents an error of the CLI that did not come through a command.
func asFailure(err error) *Failure {
	var coded *domain.Error
	if !errors.As(err, &coded) {
		coded = domain.NewError(domain.CodeInternal, err.Error(), nil)
	}
	return &Failure{Code: coded.Code, Message: coded.Message, Hint: hintFor(coded.Code, coded.Details), ExitCode: domain.ExitCode(coded.Code)}
}

func refused(code domain.Code, message string) Result {
	return Result{Failure: asFailure(domain.NewError(code, message, nil))}
}
