package harness

import (
	"bytes"
	"errors"
	"fmt"
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

const markerPrefix = "{{cf-secret:"

var secretMarker = regexp.MustCompile(`\{\{cf-secret:([A-Za-z0-9][A-Za-z0-9._/-]{0,127})\}\}`)

// SecretMarker is how ref appears in the content of a File:
// {{cf-secret:<ref>}}. A ref starts with a letter or a digit and goes on
// with letters, digits, '.', '_', '/' and '-', up to 128 in all.
func SecretMarker(ref string) string {
	return markerPrefix + ref + "}}"
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

// CheckSecrets is an error, wrapping ErrSecretUnavailable, when the secrets of
// f are not what they seem: a {{cf-secret: that opens no valid marker, which
// would otherwise be written as it is, or SecretRefs that are not
// SecretRefs(Content).
func (f File) CheckSecrets() error {
	if bytes.Count(f.Content, []byte(markerPrefix)) != len(secretMarker.FindAllIndex(f.Content, -1)) {
		return fmt.Errorf("%w: %s has a %s that is not a valid marker", ErrSecretUnavailable, f.Path, markerPrefix)
	}
	if want := SecretRefs(f.Content); !slices.Equal(f.SecretRefs, want) {
		return fmt.Errorf("%w: %s says its secret refs are %q, but its content has %q", ErrSecretUnavailable, f.Path, f.SecretRefs, want)
	}
	return nil
}
