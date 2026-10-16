package domain

// Layouts of the frozen spec of a Run (§3.3). A Run that uses no secret keeps
// RunSpecSchema, byte for byte, so its digest and the Workers that read it are
// those of before secrets; only a Run that uses one is frozen as
// RunSpecSchemaSecrets, which a Worker that predates secrets refuses.
const (
	RunSpecSchema        = 2
	RunSpecSchemaSecrets = 3
)

// RunSpecSchemaFor is the layout a Run is frozen with.
func RunSpecSchemaFor(usesSecrets bool) int {
	if usesSecrets {
		return RunSpecSchemaSecrets
	}
	return RunSpecSchema
}

// ReadableRunSpecSchema reports whether a Worker of this version reads a spec
// of the layout.
func ReadableRunSpecSchema(schema int) bool {
	return schema == RunSpecSchema || schema == RunSpecSchemaSecrets
}
