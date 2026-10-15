package harness

import (
	"errors"
	"regexp"
	"slices"
)

// SecretEnv is an environment variable whose value is a secret: Launch
// carries its Ref, and the wrapper resolves it just before exec.
type SecretEnv struct {
	Name string
	Ref  string
}

// ErrSecretUnavailable is a secret ref with nothing to resolve it
// (CF_SECRET_UNAVAILABLE). Until secrets-local, every ref is.
var ErrSecretUnavailable = errors.New("harness: secret unavailable")

var secretMarker = regexp.MustCompile(`\{\{cf-secret:([A-Za-z0-9][A-Za-z0-9._/-]{0,127})\}\}`)

// SecretMarker is how ref appears in the content of a File:
// {{cf-secret:<ref>}}. A ref starts with a letter or a digit and goes on
// with letters, digits, '.', '_', '/' and '-', up to 128 in all.
func SecretMarker(ref string) string {
	return "{{cf-secret:" + ref + "}}"
}

// SecretRefs are the refs of the markers in content, in order of first
// appearance and without repeats.
func SecretRefs(content []byte) []string {
	var refs []string
	for _, m := range secretMarker.FindAllSubmatch(content, -1) {
		if ref := string(m[1]); !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}
