package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create and manage Tasks"}
	cmd.AddCommand(
		newCreateTaskCmd("new", "Create a Task and, unless --no-start, its first Run"),
		newTaskListCmd(), newTaskShowCmd(), newTaskSetCmd(),
		newTaskActionCmd("cancel", "Cancel a Task and its live Runs", (*client.Client).CancelTask),
		newTaskActionCmd("complete", "Mark a Task completed by hand", (*client.Client).CompleteTask),
		newTaskActionCmd("archive", "Archive a Task, which frees its name", (*client.Client).ArchiveTask),
		newFutureTaskCmd("defer", "Suspend a Task's Run with a checkpoint", 1, (*client.Client).DeferTask),
		newFutureTaskCmd("resume", "Continue a deferred Task", 1, (*client.Client).ResumeTask),
		newFutureTaskCmd("move", "Continue a Task on another Worker", 2, (*client.Client).MoveTask),
	)
	return cmd
}

// newCreateTaskCmd is `cf task new` and, under another name, `cf do` (§11.1).
func newCreateTaskCmd(use, short string) *cobra.Command {
	var (
		req         v1.CreateTaskRequest
		interactive bool
		completion  string
	)
	cmd := &cobra.Command{
		Use:   use + ` "<prompt>"`,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Prompt = args[0]
			req.Completion = domain.Completion(completion)
			if interactive {
				req.Mode = domain.ModeInteractive
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.NewTask(cmd.Context(), dir, req)
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error { return printCreated(w, out) })
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Project, flagProject, "", "Project of the Task (default: the one whose repository is this directory's origin)")
	f.StringVar(&req.Name, "name", "", "name of the Task (default: made from the prompt)")
	f.StringVar(&req.SourceRef, "ref", "", "branch or tag to start from (default: the Project's default branch)")
	f.BoolVar(&req.NoStart, "no-start", false, "create the Task without a Run")
	f.BoolVarP(&interactive, "interactive", "i", false, "run it interactively instead of headless")
	f.StringVar(&completion, "completion", "", "completion policy: first_success, promote or manual")
	f.StringVar(&req.Requirements.Harness, "harness", "", "harness to run it with")
	f.StringVar(&req.Requirements.Model, "model", "", "model to run it with")
	f.StringVar(&req.RequestID, flagRequestID, "", "idempotency key: repeating the command with it creates nothing twice")
	return cmd
}

func printCreated(w io.Writer, out v1.CreateTaskResponse) error {
	t := out.Task
	if out.Run == nil {
		_, err := fmt.Fprintf(w, "Created %s %s: %s.\n", t.Short, t.Name, stateLabel(t.State, t.WaitingReason))
		return err
	}
	_, err := fmt.Fprintf(w, "Created %s %s; %s %s at %s.\n", t.Short, t.Name, out.Run.Short, out.Run.State, shorten(out.Run.SourceCommit))
	return err
}

func newTaskListCmd() *cobra.Command {
	var (
		project string
		states  []string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Tasks; archived ones only with --all or --state archived",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := client.TaskQuery{Project: project, All: all}
			for _, s := range states {
				q.States = append(q.States, domain.TaskState(s))
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			tasks, err := c.Tasks(cmd.Context(), q)
			if err != nil {
				return err
			}
			return emit(cmd, v1.ListTasksResponse{Tasks: tasks}, func(w io.Writer) error { return printTasks(w, tasks) })
		},
	}
	cmd.Flags().StringVar(&project, flagProject, "", "only the Tasks of this Project")
	cmd.Flags().StringSliceVar(&states, "state", nil, "only Tasks in these states")
	cmd.Flags().BoolVar(&all, "all", false, "include archived Tasks")
	return cmd
}

func newTaskShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <task>",
		Short: "Show a Task and its Runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			t, err := c.Task(cmd.Context(), args[0], refScope(cmd, c))
			if err != nil {
				return err
			}
			runs, err := c.Runs(cmd.Context(), client.RunQuery{Task: t.ID})
			if err != nil {
				return err
			}
			detail := struct {
				v1.Task
				Runs []v1.Run `json:"runs"`
			}{t, append([]v1.Run{}, runs...)}
			return emit(cmd, detail, func(w io.Writer) error {
				if err := printTask(w, t); err != nil {
					return err
				}
				if len(runs) == 0 {
					return nil
				}
				fmt.Fprintln(w)
				return printRuns(w, runs)
			})
		},
	}
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

func printTask(w io.Writer, t v1.Task) error {
	_, err := fmt.Fprintf(w, "ID          %s (%s)\nName        %s\nState       %s\nSource      %s\nMode        %s\nCompletion  %s\nCreated     %s\nPrompt      %s\n",
		t.ID, t.Short, t.Name, stateLabel(t.State, t.WaitingReason), t.Source.Ref, t.Mode, t.Completion, local(t.CreatedAt), t.Prompt)
	return err
}

func newTaskSetCmd() *cobra.Command {
	var name, completion string
	cmd := &cobra.Command{
		Use:   "set <task>",
		Short: "Rename a Task or change its completion policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req v1.UpdateTaskRequest
			if cmd.Flags().Changed("name") {
				req.Name = &name
			}
			if cmd.Flags().Changed("completion") {
				c := domain.Completion(completion)
				req.Completion = &c
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			t, err := c.UpdateTask(cmd.Context(), args[0], refScope(cmd, c), req)
			if err != nil {
				return err
			}
			return emit(cmd, t, func(w io.Writer) error { return printTask(w, t) })
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "new name")
	cmd.Flags().StringVar(&completion, "completion", "", "completion policy: first_success, promote or manual")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

func newTaskActionCmd(verb, short string, act func(*client.Client, context.Context, string, string) (v1.Task, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   verb + " <task>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			t, err := act(c, cmd.Context(), args[0], refScope(cmd, c))
			if err != nil {
				return err
			}
			return emit(cmd, t, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s is now %s.\n", t.Name, stateLabel(t.State, t.WaitingReason))
				return err
			})
		},
	}
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

// newFutureTaskCmd is a command of §18.2 whose milestone has not arrived: it
// exists, takes the arguments it will take, and the Controller answers
// CF_NOT_IMPLEMENTED (exit 8). Flags it will gain are accepted and ignored.
func newFutureTaskCmd(verb, short string, args int, call func(*client.Client, context.Context, string, string) error) *cobra.Command {
	use := verb + " <task>"
	if args == 2 {
		use += " <worker>"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short + " (not implemented yet)",
		Args:  cobra.MinimumNArgs(args),
		RunE: func(cmd *cobra.Command, a []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			return call(c, cmd.Context(), a[0], refScope(cmd, c))
		},
	}
	cmd.FParseErrWhitelist.UnknownFlags = true
	addProjectFlag(cmd, "Project that narrows a name more than one Project has")
	return cmd
}
