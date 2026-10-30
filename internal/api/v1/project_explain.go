package v1

import "github.com/Autarquico/codefabric/internal/domain"

// ExplainProjectRequest is POST /v1/projects/{ref}/explain (§3.1). The client
// reads the repo, because the Controller never touches the origin (ADR 0002
// D1): it sends the commit it resolved, the bytes of .codefabric/project.yaml
// at that commit (FilePresent says whether the commit has one; an empty file
// is present), and the git blob id of each digest input it was asked for.
// BlobIDs maps a path to its blob id, or to "" when the commit lacks it; a
// path that is not in the map has not been looked up yet.
type ExplainProjectRequest struct {
	Commit      string            `json:"commit"`
	FilePresent bool              `json:"file_present"`
	File        []byte            `json:"file,omitempty"`
	BlobIDs     map[string]string `json:"blob_ids,omitempty"`
}

// ExplainedField is one effective value and the layer that gave it: global,
// project, repo or clamped (a repo request above the Controller's ceiling).
type ExplainedField struct {
	Name   string        `json:"name"`
	Value  any           `json:"value"`
	Origin domain.Origin `json:"origin"`
}

// ProjectExplanation is the answer: the effective configuration of the
// Project at a commit, without a Task. Invalid is set instead of Fields when
// the file of the repo is not valid (CF_REPO_CONFIG_INVALID); NeedBlobs when
// the digest needs blob ids the request did not carry, in which case the
// request is to be repeated with them.
type ProjectExplanation struct {
	Project         string            `json:"project"`
	Commit          string            `json:"commit"`
	RepoConfig      domain.RepoConfig `json:"repo_config"`
	FilePresent     bool              `json:"file_present"`
	FileIgnored     bool              `json:"file_ignored"`
	Fields          []ExplainedField  `json:"fields"`
	EffectiveDigest string            `json:"effective_digest,omitempty"`
	NeedBlobs       []string          `json:"need_blobs,omitempty"`
	Invalid         *ErrorBody        `json:"invalid,omitempty"`
}
