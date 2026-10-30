package projects

import (
	"context"
	"encoding/json"
	"errors"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/environments"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
)

// Explain is the configuration a Run of the Project would have at the commit
// of req, with the layer that gave each field (§3.1): what a Task leaves
// unset (as for any Task), the Environment the Project names at its latest
// version, and the file of the repo if the Project allows it. It writes
// nothing and never reads the repo: the client sends the file.
//
// An invalid file is not an error of the call: it is answered in Invalid, with
// the code and detail the Worker would fail the Run with.
func (s *Service) Explain(ctx context.Context, ref string, req v1.ExplainProjectRequest) (v1.ProjectExplanation, error) {
	if !domain.ValidCommitID(req.Commit) {
		return v1.ProjectExplanation{}, input.Invalid("commit must be a full commit ID")
	}
	var out v1.ProjectExplanation
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := refs.Project(ctx, q, ref)
		if err != nil {
			return err
		}
		p, err := store.ProjectByID(ctx, q, id)
		if err != nil {
			return err
		}
		in, origins := runs.InheritWithOrigins(store.Task{}, p)
		inputs := repoconfig.Inputs{
			RepoConfig: p.RepoConfig, CPUOrigin: origins.Resources, MemoryOrigin: origins.Resources,
			EnvironmentOrigin: origins.Environment,
		}
		if r := in.Requirements.Resources; r != nil {
			inputs.Resources = domain.Resources{CPU: r.CPU, MemoryMB: r.MemoryMB}
		}
		var pin string
		if in.Environment != "" {
			if pin, inputs.Environment, err = latestEnvironment(ctx, q, in.Environment); err != nil {
				return err
			}
		}
		out = v1.ProjectExplanation{
			Project: p.Name, Commit: req.Commit, RepoConfig: p.RepoConfig,
			FilePresent: req.FilePresent, FileIgnored: req.FilePresent && p.RepoConfig == domain.RepoConfigDeny,
		}
		if req.FilePresent && p.RepoConfig != domain.RepoConfigDeny {
			file, err := repoconfig.Parse(req.File)
			if err != nil {
				return invalidFile(&out, err)
			}
			inputs.File = &file
		}
		eff, err := repoconfig.Resolve(inputs)
		if err != nil {
			return err
		}
		out.Fields = fields(in, origins, pin, eff)
		for _, path := range eff.DigestInputPaths() {
			if _, ok := req.BlobIDs[path]; !ok {
				out.NeedBlobs = append(out.NeedBlobs, path)
			}
		}
		if len(out.NeedBlobs) == 0 {
			out.EffectiveDigest, err = repoconfig.EffectiveDigest(eff, req.BlobIDs)
		}
		return err
	})
	return out, err
}

// invalidFile records a coded parse error in out and is nil; any other error
// is a bug and goes up.
func invalidFile(out *v1.ProjectExplanation, err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	out.Invalid = &v1.ErrorBody{Code: de.Code, Message: de.Message, Details: de.Details}
	return nil
}

// latestEnvironment reads the head version of the named Environment, or the
// version the selector names, without pinning anything.
func latestEnvironment(ctx context.Context, q store.Querier, selector string) (string, *domain.EnvironmentDef, error) {
	ref, err := environments.Resolve(ctx, q, selector)
	if err != nil {
		return "", nil, err
	}
	v, err := store.EnvironmentVersionByNumber(ctx, q, ref.ID, ref.Version)
	if err != nil {
		return "", nil, err
	}
	var def domain.EnvironmentDef
	if err := json.Unmarshal(v.Definition, &def); err != nil {
		return "", nil, err
	}
	return ref.ID + "@" + ref.Digest, &def, nil
}

func fields(in runs.Inherited, o runs.Origins, environmentPin string, eff repoconfig.Effective) []v1.ExplainedField {
	out := []v1.ExplainedField{
		{Name: "source_ref", Value: in.SourceRef, Origin: o.SourceRef},
		{Name: "mode", Value: in.Mode, Origin: o.Mode},
		{Name: "workflow", Value: in.Workflow, Origin: o.Workflow},
		{Name: "environment", Value: environmentPin, Origin: o.Environment},
		{Name: "completion", Value: in.Completion, Origin: o.Completion},
		{Name: "retry", Value: in.Retry, Origin: o.Retry},
		{Name: repoconfig.FieldCPU, Value: eff.Resources.CPU, Origin: eff.Origins[repoconfig.FieldCPU]},
		{Name: repoconfig.FieldMemoryMB, Value: eff.Resources.MemoryMB, Origin: eff.Origins[repoconfig.FieldMemoryMB]},
	}
	if eff.Evaluation != nil {
		out = append(out, v1.ExplainedField{Name: repoconfig.FieldEvaluation, Value: eff.Evaluation, Origin: eff.Origins[repoconfig.FieldEvaluation]})
	}
	if env := eff.Environment; env != nil {
		o := eff.Origins
		out = append(out,
			v1.ExplainedField{Name: repoconfig.FieldProvider, Value: env.Provider, Origin: o[repoconfig.FieldProvider]},
			v1.ExplainedField{Name: repoconfig.FieldRequiresCapabilities, Value: env.RequiresCapabilities, Origin: o[repoconfig.FieldRequiresCapabilities]},
			v1.ExplainedField{Name: repoconfig.FieldPrepare, Value: env.Prepare, Origin: o[repoconfig.FieldPrepare]},
			v1.ExplainedField{Name: repoconfig.FieldServices, Value: env.Services, Origin: o[repoconfig.FieldServices]},
			v1.ExplainedField{Name: repoconfig.FieldDigestInputs, Value: env.DigestInputs, Origin: o[repoconfig.FieldDigestInputs]},
		)
	}
	return out
}
