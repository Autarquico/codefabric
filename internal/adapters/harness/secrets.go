package harness

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
)

// SecretEnv is an environment variable whose value is a secret: Launch
// carries its Ref, and the wrapper resolves it just before exec.
type SecretEnv struct {
	Name string
	Ref  string
}

// ErrSecretUnavailable is a secret ref with nothing to resolve it
// (CF_SECRET_UNAVAILABLE).
var ErrSecretUnavailable = errors.New("harness: secret unavailable")

const markerPrefix = "{{cf-secret:"

// marker is anything shaped like a marker; only those whose ref is
// domain.ValidSecretRef are markers.
var marker = regexp.MustCompile(`\{\{cf-secret:([^{}]*)\}\}`)

// SecretMarker is how ref appears in the content of a File:
// {{cf-secret:<ref>}}, with ref as domain.ValidSecretRef has it.
func SecretMarker(ref string) string {
	return markerPrefix + ref + "}}"
}

// SecretRefs are the refs of the markers in content, in order of first
// appearance and without repeats.
func SecretRefs(content []byte) []string {
	refs, _ := markers(content)
	return refs
}

// markers are the refs of the markers in content, in order of first
// appearance and without repeats, and how many there are, repeats included.
func markers(content []byte) (refs []string, count int) {
	for _, m := range marker.FindAllSubmatch(content, -1) {
		ref := string(m[1])
		if !domain.ValidSecretRef(ref) {
			continue
		}
		count++
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs, count
}

// CheckSecrets is an error, wrapping ErrSecretUnavailable, when the secrets of
// f are not what they seem: a {{cf-secret: that opens no valid marker, which
// would otherwise be written as it is, or SecretRefs that are not
// SecretRefs(Content).
func (f File) CheckSecrets() error {
	refs, count := markers(f.Content)
	if bytes.Count(f.Content, []byte(markerPrefix)) != count {
		return fmt.Errorf("%w: %s has a %s that is not a valid marker", ErrSecretUnavailable, f.Path, markerPrefix)
	}
	if !slices.Equal(f.SecretRefs, refs) {
		return fmt.Errorf("%w: %s says its secret refs are %q, but its content has %q", ErrSecretUnavailable, f.Path, f.SecretRefs, refs)
	}
	return nil
}
