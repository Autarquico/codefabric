// Package projects is the use cases of Projects (§3.1): create, list, show and
// change their defaults.
package projects

import (
	"context"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const defaultBranch = "main"

// Service runs the Project use cases.
type Service struct {
	db    *store.DB
	clock clock.Clock
	ids   *domain.IDGenerator
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator) *Service {
	return &Service{db: db, clock: clk, ids: ids}
}

// Create registers a Project. Names are unique among Projects.
func (s *Service) Create(ctx context.Context, by pki.Session, req v1.CreateProjectRequest) (v1.Project, error) {
	if req.DefaultBranch == "" {
		req.DefaultBranch = defaultBranch
	}
	if req.RepoConfig == "" {
		req.RepoConfig = domain.Defaults().ProjectRepoConfig
	}
	if err := validateNew(req); err != nil {
		return v1.Project{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "project.create", req.RequestID, req,
		func(tx *store.Tx, act mutation.Act) (v1.Project, error) {
			if taken, err := store.ProjectNameTaken(ctx, tx, req.Name); err != nil || taken {
				if err != nil {
					return v1.Project{}, err
				}
				return v1.Project{}, input.Invalid("a Project named %q already exists", req.Name)
			}
			short, err := tx.NextShortID(ctx, domain.KindProject)
			if err != nil {
				return v1.Project{}, err
			}
			p := store.Project{
				ID: s.ids.New(domain.KindProject), Short: short, Name: req.Name, Repo: req.Repo,
				DefaultBranch: req.DefaultBranch, DefaultWorkflow: req.DefaultWorkflow,
				DefaultEnvironment: req.DefaultEnvironment, Defaults: req.Defaults, RepoConfig: req.RepoConfig, CreatedAt: act.Now,
			}
			if err := tx.InsertProject(ctx, p); err != nil {
				return v1.Project{}, err
			}
			view := View(p)
			return view, audit(ctx, tx, act, "project.created", p.ID, nil, view)
		})
}

// Set changes the fields of a Project that req carries.
func (s *Service) Set(ctx context.Context, by pki.Session, ref string, req v1.UpdateProjectRequest) (v1.Project, error) {
	if err := validateUpdate(req); err != nil {
		return v1.Project{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "project.set", req.RequestID, struct {
		Ref string `json:"ref"`
		v1.UpdateProjectRequest
	}{ref, req}, func(tx *store.Tx, act mutation.Act) (v1.Project, error) {
		id, err := refs.Project(ctx, tx, ref)
		if err != nil {
			return v1.Project{}, err
		}
		p, err := store.ProjectByID(ctx, tx, id)
		if err != nil {
			return v1.Project{}, err
		}
		before := View(p)
		if err := apply(&p, req); err != nil {
			return v1.Project{}, err
		}
		if err := tx.UpdateProject(ctx, p); err != nil {
			return v1.Project{}, err
		}
		after := View(p)
		return after, audit(ctx, tx, act, "project.updated", p.ID, before, after)
	})
}

// List returns every Project, oldest first.
func (s *Service) List(ctx context.Context) ([]v1.Project, error) {
	var out []v1.Project
	err := s.db.Read(ctx, func(q store.Querier) error {
		rows, err := store.Projects(ctx, q)
		for _, p := range rows {
			out = append(out, View(p))
		}
		return err
	})
	return out, err
}

// Get returns the Project a reference points to.
func (s *Service) Get(ctx context.Context, ref string) (v1.Project, error) {
	var out v1.Project
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := refs.Project(ctx, q, ref)
		if err != nil {
			return err
		}
		p, err := store.ProjectByID(ctx, q, id)
		out = View(p)
		return err
	})
	return out, err
}

// View is the API shape of a Project.
func View(p store.Project) v1.Project {
	return v1.Project{
		ID: string(p.ID), Short: domain.ShortID{Kind: domain.KindProject, N: p.Short}.String(),
		Name: p.Name, Repo: p.Repo, RepoAliases: append([]string{}, p.RepoAliases...), DefaultBranch: p.DefaultBranch,
		DefaultWorkflow: p.DefaultWorkflow, DefaultEnvironment: p.DefaultEnvironment,
		Defaults: p.Defaults, RepoConfig: p.RepoConfig, CreatedAt: p.CreatedAt,
	}
}

func apply(p *store.Project, req v1.UpdateProjectRequest) error {
	if err := editAliases(p, req.AddRepos, req.RemoveRepos); err != nil {
		return err
	}
	if req.DefaultBranch != nil {
		p.DefaultBranch = *req.DefaultBranch
	}
	if req.DefaultWorkflow != nil {
		p.DefaultWorkflow = *req.DefaultWorkflow
	}
	if req.DefaultEnvironment != nil {
		p.DefaultEnvironment = *req.DefaultEnvironment
	}
	if req.RepoConfig != nil {
		p.RepoConfig = *req.RepoConfig
	}
	if req.Mode != nil {
		p.Defaults.Mode = *req.Mode
	}
	if req.Resources != nil {
		p.Defaults.Resources = req.Resources
	}
	if req.Completion != nil {
		p.Defaults.Completion = *req.Completion
	}
	if req.Retry != nil {
		p.Defaults.Retry = req.Retry
	}
	return nil
}

// editAliases removes, then adds, URLs among the aliases of p. The URLs are
// kept as given: they are only ever compared literally. Adding a URL the
// Project already has changes nothing.
func editAliases(p *store.Project, add, remove []string) error {
	for _, url := range remove {
		i := slices.Index(p.RepoAliases, url)
		switch {
		case url == p.Repo:
			return input.Invalid("the main repo of a Project cannot be removed")
		case i < 0:
			return input.Invalid("remove_repos has a URL that is not a repo alias of this Project")
		}
		p.RepoAliases = slices.Delete(p.RepoAliases, i, i+1)
	}
	for _, url := range add {
		if url != p.Repo && !slices.Contains(p.RepoAliases, url) {
			p.RepoAliases = append(p.RepoAliases, url)
		}
	}
	if len(p.RepoAliases) > maxRepoAliases {
		return input.Invalid("a Project has at most %d repo aliases", maxRepoAliases)
	}
	slices.Sort(p.RepoAliases)
	return nil
}

// maxRepoAliases bounds the URLs a Project matches; real clones need a handful.
const maxRepoAliases = 32

func validateNew(req v1.CreateProjectRequest) error {
	return input.First(
		input.Name("name", req.Name), input.Repo(req.Repo), input.Ref("default_branch", req.DefaultBranch),
		input.Label("default_workflow", req.DefaultWorkflow), input.Label("default_environment", req.DefaultEnvironment),
		repoConfig(req.RepoConfig),
		validateDefaults(req.Defaults.Mode, req.Defaults.Resources, req.Defaults.Completion, req.Defaults.Retry),
	)
}

func validateUpdate(req v1.UpdateProjectRequest) error {
	var errs []error
	for _, url := range req.AddRepos {
		errs = append(errs, input.Repo(url))
	}
	for _, url := range req.RemoveRepos {
		if url == "" || slices.Contains(req.AddRepos, url) {
			errs = append(errs, input.Invalid("remove_repos must be non-empty and not also in add_repos"))
		}
	}
	if req.DefaultBranch != nil {
		errs = append(errs, input.Ref("default_branch", *req.DefaultBranch))
	}
	if req.DefaultWorkflow != nil {
		errs = append(errs, input.Label("default_workflow", *req.DefaultWorkflow))
	}
	if req.DefaultEnvironment != nil {
		errs = append(errs, input.Label("default_environment", *req.DefaultEnvironment))
	}
	if req.RepoConfig != nil {
		errs = append(errs, repoConfig(*req.RepoConfig))
	}
	var mode domain.Mode
	var completion domain.Completion
	if req.Mode != nil {
		mode = *req.Mode
	}
	if req.Completion != nil {
		completion = *req.Completion
	}
	return input.First(append(errs, validateDefaults(mode, req.Resources, completion, req.Retry))...)
}

func repoConfig(c domain.RepoConfig) error {
	if c != domain.RepoConfigAllow && c != domain.RepoConfigDeny {
		return input.Invalid("repo_config must be allow or deny")
	}
	return nil
}

func validateDefaults(mode domain.Mode, resources *v1.Resources, completion domain.Completion, retry *v1.Retry) error {
	return input.First(input.Mode(mode), input.Resources(resources), input.Completion(completion), input.Retry(retry))
}

func audit(ctx context.Context, tx *store.Tx, act mutation.Act, action string, id domain.ID, before, after any) error {
	return tx.AppendAudit(ctx, store.AuditEntry{
		Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: action,
		ResourceKind: "project", ResourceID: string(id), Before: before, After: after, Result: "ok",
	})
}
