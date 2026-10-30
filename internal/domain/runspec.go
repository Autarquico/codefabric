package domain

// Layouts of the frozen spec of a Run (§3.3). A Run that uses no secret keeps
// RunSpecSchema, byte for byte, so its digest and the Workers that read it are
// those of before secrets; only a Run that uses one is frozen as
// RunSpecSchemaSecrets, which a Worker that predates secrets refuses.
// RunSpecSchemaRepoConfig adds the repo configuration and the Environment of
// the Run (§25); it is used only by a Run whose Project allows repo_config or
// that names an Environment, and a Worker that predates it refuses it.
const (
	RunSpecSchema           = 2
	RunSpecSchemaSecrets    = 3
	RunSpecSchemaRepoConfig = 4
)

// RunSpecSchemaFor is the layout a Run is frozen with.
func RunSpecSchemaFor(usesSecrets bool) int {
	if usesSecrets {
		return RunSpecSchemaSecrets
	}
	return RunSpecSchema
}

// RunSpecSchemaWith is the layout a Run is frozen with once the repo
// configuration is a factor: the newest layout any of its features needs.
func RunSpecSchemaWith(usesSecrets, usesRepoConfig bool) int {
	if usesRepoConfig {
		return RunSpecSchemaRepoConfig
	}
	return RunSpecSchemaFor(usesSecrets)
}

// ReadableRunSpecSchema reports whether a Worker of this version reads a spec
// of the layout.
func ReadableRunSpecSchema(schema int) bool {
	return schema == RunSpecSchema || schema == RunSpecSchemaSecrets || schema == RunSpecSchemaRepoConfig
}
