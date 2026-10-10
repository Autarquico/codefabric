package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/domain"
)

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Manage Projects"}
	cmd.AddCommand(newProjectCreateCmd(), newProjectListCmd(), newProjectShowCmd(), newProjectSetCmd())
	return cmd
}

func newProjectCreateCmd() *cobra.Command {
	var req v1.CreateProjectRequest
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a Project; its repository defaults to the origin of this directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			if req.Repo == "" {
				dir, err := os.Getwd()
				if err != nil {
					return err
				}
				if req.Repo, err = gitctx.OriginURL(cmd.Context(), dir); err != nil {
					return domain.NewError(domain.CodeInvalidRequest, "this directory has no origin remote: give the repository with --repo", nil)
				}
				if !domain.ValidRepoURL(req.Repo) {
					return domain.NewError(domain.CodeInvalidRequest,
						"the origin of this directory must be "+domain.RepoFormHint+": change it (git remote set-url origin) or give the repository with --repo", nil)
				}
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			p, err := c.CreateProject(cmd.Context(), req)
			if err != nil {
				return err
			}
			return emit(cmd, p, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Created %s %s for %s.\n", p.Short, p.Name, p.Repo)
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Repo, "repo", "", "git URL of the repository (default: the origin of this directory)")
	f.StringVar(&req.DefaultBranch, "default-branch", "", "branch Tasks start from (default main)")
	f.StringVar(&req.DefaultWorkflow, "workflow", "", "default Workflow")
	f.StringVar(&req.DefaultEnvironment, "environment", "", "default Environment")
	f.StringVar(&req.RequestID, flagRequestID, "", "idempotency key: repeating the command with it creates nothing twice")
	return cmd
}

func newProjectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			projects, err := c.Projects(cmd.Context())
			if err != nil {
				return err
			}
			if projects == nil {
				projects = []v1.Project{}
			}
			return emit(cmd, v1.ListProjectsResponse{Projects: projects}, func(out io.Writer) error {
				w := newTable(out, "ID\tNAME\tBRANCH\tREPO")
				for _, p := range projects {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Short, p.Name, p.DefaultBranch, p.Repo)
				}
				return w.Flush()
			})
		},
	}
}

func newProjectShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <project>",
		Short: "Show a Project and its defaults",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			p, err := c.Project(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, p, func(w io.Writer) error { return printProject(w, p) })
		},
	}
}

func printProject(w io.Writer, p v1.Project) error {
	d := p.Defaults
	_, err := fmt.Fprintf(w, "ID           %s (%s)\nName         %s\nRepo         %s\n%sBranch       %s\nWorkflow     %s\nEnvironment  %s\nMode         %s\nCompletion   %s\nResources    %s\nRetry        %s\nCreated      %s\n",
		p.ID, p.Short, p.Name, p.Repo, aliasLines(p.RepoAliases), p.DefaultBranch, dash(p.DefaultWorkflow), dash(p.DefaultEnvironment),
		dash(string(d.Mode)), dash(string(d.Completion)), resourcesLabel(d.Resources), retryLabel(d.Retry), local(p.CreatedAt))
	return err
}

func aliasLines(aliases []string) string {
	var b strings.Builder
	for _, url := range aliases {
		fmt.Fprintf(&b, "             %s\n", url)
	}
	return b.String()
}

func resourcesLabel(r *v1.Resources) string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("%d cpu, %d MB", r.CPU, r.MemoryMB)
}

func retryLabel(r *v1.Retry) string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("on_failed=%s on_lost=%s max_attempts=%d", r.OnFailed, r.OnLost, r.MaxAttempts)
}

func newProjectSetCmd() *cobra.Command {
	var (
		branch, workflow, environment string
		mode, completion              string
		cpu, memoryMB                 int
		addRepos, removeRepos         []string
	)
	cmd := &cobra.Command{
		Use:   "set <project>",
		Short: "Change a Project's defaults; only the flags given change",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			req := v1.UpdateProjectRequest{AddRepos: addRepos, RemoveRepos: removeRepos}
			if f.Changed("default-branch") {
				req.DefaultBranch = &branch
			}
			if f.Changed("workflow") {
				req.DefaultWorkflow = &workflow
			}
			if f.Changed("environment") {
				req.DefaultEnvironment = &environment
			}
			if f.Changed("mode") {
				m := domain.Mode(mode)
				req.Mode = &m
			}
			if f.Changed("completion") {
				c := domain.Completion(completion)
				req.Completion = &c
			}
			if f.Changed("cpu") || f.Changed("memory-mb") {
				if !f.Changed("cpu") || !f.Changed("memory-mb") {
					return usageError(fmt.Errorf("--cpu and --memory-mb go together"))
				}
				req.Resources = &v1.Resources{CPU: cpu, MemoryMB: memoryMB}
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			p, err := c.UpdateProject(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			return emit(cmd, p, func(w io.Writer) error { return printProject(w, p) })
		},
	}
	f := cmd.Flags()
	f.StringVar(&branch, "default-branch", "", "branch Tasks start from")
	f.StringVar(&workflow, "workflow", "", "default Workflow (empty clears it)")
	f.StringVar(&environment, "environment", "", "default Environment (empty clears it)")
	f.StringVar(&mode, "mode", "", "default mode: headless or interactive")
	f.StringVar(&completion, "completion", "", "default completion: first_success, promote or manual")
	f.StringArrayVar(&addRepos, "add-repo", nil, "another URL of the repository; repeatable. A working copy belongs to the Project if its origin is exactly one of its URLs")
	f.StringArrayVar(&removeRepos, "remove-repo", nil, "an added URL to remove; repeatable. The main repo cannot be removed")
	f.IntVar(&cpu, "cpu", 0, "default CPUs per Run")
	f.IntVar(&memoryMB, "memory-mb", 0, "default memory per Run, in MB")
	return cmd
}
