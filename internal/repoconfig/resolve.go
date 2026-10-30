package repoconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
)

// MaxEffectiveBytes bounds the effective configuration once marshaled; it is
// v1.MaxEffectiveConfigBytes, which the Controller enforces on the report (the
// executor tests keep both equal: this package may not import the API).
const MaxEffectiveBytes = 16 << 10

// Names of the fields of Effective.Origins.
const (
	FieldCPU                  = "resources.cpu"
	FieldMemoryMB             = "resources.memory_mb"
	FieldEvaluation           = "evaluation"
	FieldProvider             = "environment.provider"
	FieldRequiresCapabilities = "environment.requires_capabilities"
	FieldPrepare              = "environment.prepare"
	FieldServices             = "environment.services"
	FieldDigestInputs         = "environment.digest_inputs"
)

// Inputs is what the caller knows before the repo speaks: the values the
// Controller resolved (task, then project, then global) with where each one
// came from, and the file of the repo if there is one. The Controller and the
// Worker build it from the same frozen facts.
type Inputs struct {
	// RepoConfig is the policy of the Project; with deny the File is ignored.
	// The caller should not even parse the file then, so that an invalid one
	// cannot fail the Run.
	RepoConfig domain.RepoConfig

	// Resources is the ceiling the Controller resolved, each dimension with
	// its origin (global, project or task).
	Resources    domain.Resources
	CPUOrigin    domain.Origin
	MemoryOrigin domain.Origin

	// Environment is the definition the Run pinned, nil if it has none, and
	// EnvironmentOrigin where it was chosen from.
	Environment       *domain.EnvironmentDef
	EnvironmentOrigin domain.Origin

	// File is the parsed project.yaml at the commit of the Run, nil if the repo
	// has none.
	File *File
}

// Resources is the CPU and memory of a Run, in the shape that is stored and
// hashed.
type Resources struct {
	CPU      int `json:"cpu"`
	MemoryMB int `json:"memory_mb"`
}

// Effective is the configuration a Run runs with.
type Effective struct {
	Resources   Resources              `json:"resources"`
	Environment *domain.EnvironmentDef `json:"environment"`
	Evaluation  map[string]any         `json:"evaluation"`
	// Origins says where each field came from; it is provenance, not content,
	// and stays out of the digest.
	Origins map[string]domain.Origin `json:"origins"`
	// FileIgnored is set when the repo has a file and the Project denies it.
	FileIgnored bool `json:"file_ignored"`
}

// Resolve merges the file of the repo over the configuration of the
// Controller, field by field. The repo can lower resources, never raise them
// (a higher request is clamped to the Controller's value and marked so), and
// it cannot touch the provider or the capabilities of an Environment. A result
// over MaxEffectiveBytes once marshaled is CodeRepoConfigInvalid, for
// the Worker and for explain alike.
func Resolve(in Inputs) (Effective, error) {
	eff, err := resolve(in)
	if err != nil {
		return Effective{}, err
	}
	raw, err := json.Marshal(eff)
	if err != nil {
		return Effective{}, err
	}
	if len(raw) > MaxEffectiveBytes {
		return Effective{}, domain.NewError(domain.CodeRepoConfigInvalid,
			fmt.Sprintf("the effective configuration is over %d bytes", MaxEffectiveBytes), nil)
	}
	return eff, nil
}

func resolve(in Inputs) (Effective, error) {
	for _, o := range []domain.Origin{in.CPUOrigin, in.MemoryOrigin} {
		if !controllerOrigin(o) {
			return Effective{}, fmt.Errorf("repoconfig: resources need a controller origin, got %q", o)
		}
	}
	if in.Environment != nil && !controllerOrigin(in.EnvironmentOrigin) {
		return Effective{}, fmt.Errorf("repoconfig: environment needs a controller origin, got %q", in.EnvironmentOrigin)
	}
	eff := Effective{
		Resources: Resources{CPU: in.Resources.CPU, MemoryMB: in.Resources.MemoryMB},
		Origins:   map[string]domain.Origin{FieldCPU: in.CPUOrigin, FieldMemoryMB: in.MemoryOrigin},
	}
	if in.Environment != nil {
		env := cloneEnvironment(*in.Environment)
		eff.Environment = &env
		for _, f := range environmentFields {
			eff.Origins[f] = in.EnvironmentOrigin
		}
	}
	file := in.File
	if file == nil {
		return eff, nil
	}
	if in.RepoConfig == domain.RepoConfigDeny {
		eff.FileIgnored = true
		return eff, nil
	}
	if file.Evaluation != nil {
		eff.Evaluation = file.Evaluation
		eff.Origins[FieldEvaluation] = domain.OriginRepo
	}
	if r := file.Resources; r != nil {
		eff.Resources.CPU, eff.Origins[FieldCPU] = lower(in.Resources.CPU, in.CPUOrigin, r.CPU)
		eff.Resources.MemoryMB, eff.Origins[FieldMemoryMB] = lower(in.Resources.MemoryMB, in.MemoryOrigin, r.MemoryMB)
	}
	if o := file.Environment; o != nil {
		mergeEnvironment(&eff, *o)
	}
	return eff, nil
}

var environmentFields = []string{FieldProvider, FieldRequiresCapabilities, FieldPrepare, FieldServices, FieldDigestInputs}

func controllerOrigin(o domain.Origin) bool {
	return o == domain.OriginGlobal || o == domain.OriginProject || o == domain.OriginTask
}

// lower is the value a request leaves under a ceiling and where it came from.
func lower(ceiling int, ceilingOrigin domain.Origin, asked *int) (int, domain.Origin) {
	switch {
	case asked == nil:
		return ceiling, ceilingOrigin
	case *asked > ceiling:
		return ceiling, domain.OriginClamped
	default:
		return *asked, domain.OriginRepo
	}
}

// mergeEnvironment lets the repo replace the fields it may set. Without an
// Environment of the Controller the repo defines a whole one with the host
// provider, and every field of it comes from the repo.
func mergeEnvironment(eff *Effective, o EnvironmentOverride) {
	if eff.Environment == nil {
		eff.Environment = &domain.EnvironmentDef{Provider: domain.ProviderHost}
		for _, f := range environmentFields {
			eff.Origins[f] = domain.OriginRepo
		}
	}
	env := eff.Environment
	if o.Prepare != nil {
		env.Prepare = *o.Prepare
		eff.Origins[FieldPrepare] = domain.OriginRepo
	}
	if o.Services != nil {
		env.Services = slices.Clone(*o.Services)
		eff.Origins[FieldServices] = domain.OriginRepo
	}
	if o.DigestInputs != nil {
		env.DigestInputs = slices.Clone(*o.DigestInputs)
		eff.Origins[FieldDigestInputs] = domain.OriginRepo
	}
}

func cloneEnvironment(d domain.EnvironmentDef) domain.EnvironmentDef {
	d.RequiresCapabilities = slices.Clone(d.RequiresCapabilities)
	d.Services = slices.Clone(d.Services)
	d.DigestInputs = slices.Clone(d.DigestInputs)
	return d
}

// DigestInputPaths are the paths whose blob ids EffectiveDigest needs, sorted.
func (e Effective) DigestInputPaths() []string {
	if e.Environment == nil {
		return nil
	}
	return sortedCopy(e.Environment.DigestInputs)
}

// Absent is how the digest spells a digest input the commit does not have.
const Absent = "absent"

// EffectiveDigest is "sha256:<hex>" over the canonical effective configuration
// and the (path, blob id) of each of its digest inputs at the commit of the
// Run. blobIDs maps a path to the git blob id there; a path missing from it
// (or empty) is absent. Paths that are not digest inputs are ignored, and
// nothing is read from a worktree, so that the digest depends on the commit
// alone.
func EffectiveDigest(eff Effective, blobIDs map[string]string) (string, error) {
	type input struct {
		Path string `json:"path"`
		Blob string `json:"blob"`
	}
	type content struct {
		Resources   Resources       `json:"resources"`
		Environment json.RawMessage `json:"environment"`
		Evaluation  map[string]any  `json:"evaluation"`
		Inputs      []input         `json:"inputs"`
	}
	c := content{Resources: eff.Resources, Evaluation: eff.Evaluation, Inputs: []input{}, Environment: json.RawMessage("null")}
	if eff.Environment != nil {
		canonical, err := eff.Environment.Canonical()
		if err != nil {
			return "", err
		}
		c.Environment = canonical
	}
	for _, p := range eff.DigestInputPaths() {
		blob := blobIDs[p]
		if blob == "" {
			blob = Absent
		}
		c.Inputs = append(c.Inputs, input{Path: p, Blob: blob})
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
