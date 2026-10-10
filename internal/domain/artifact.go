package domain

// ArtifactType classifies a durable Run output (§24).
type ArtifactType string

// Artifact types.
const (
	ArtifactPatch      ArtifactType = "patch"
	ArtifactCommit     ArtifactType = "commit"
	ArtifactFile       ArtifactType = "file"
	ArtifactTestReport ArtifactType = "test-report"
	ArtifactBenchmark  ArtifactType = "benchmark"
	ArtifactBuild      ArtifactType = "build"
	ArtifactScreenshot ArtifactType = "screenshot"
	ArtifactOther      ArtifactType = "other"
)

// Artifact is a durable output of a Run (§24).
type Artifact struct {
	ID        ID
	RunID     ID
	Type      ArtifactType
	Name      string
	Digest    string
	Size      int64
	MediaType string
	Retention string
}
