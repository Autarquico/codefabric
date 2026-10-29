package harness

import (
	"errors"
	"fmt"
	"slices"
	"strings"

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

// SecretMarker is how ref appears in the content of a File:
// {{cf-secret:<ref>}}, with ref as domain.ValidSecretRef has it.
func SecretMarker(ref string) string {
	return domain.SecretMarkerPrefix + ref + "}}"
}

// SecretRefs are the refs of the markers in content: domain.SecretRefsIn, the
// one grammar of a marker.
func SecretRefs(content []byte) []string {
	return domain.SecretRefsIn(content)
}

// Escape is how a secret value is written inside a File so that the format of
// the file reads back exactly that value. The marker of a secret stands where
// the value will be; the escape is what the value goes through there.
type Escape string

// The escapes a File may ask for.
const (
	// EscapeRaw writes the value as it is.
	EscapeRaw Escape = "raw"
	// EscapeTOMLBasicString escapes the value for the inside of a TOML basic
	// string ("..."): the marker must stand between its quotes, never in a
	// literal string, a multi-line string or a bare value.
	EscapeTOMLBasicString Escape = "toml-basic-string"
)

// SecretEscaper is implemented by an adapter whose configuration files carry
// markers: it says, for each file of its snapshots, how a value is escaped
// in it. The port knows no file format; the Worker renders the markers of a
// Run's configuration with the escape the adapter names, and refuses a file
// with markers whose adapter names none.
type SecretEscaper interface {
	SecretEscape(path string) (Escape, error)
}

// RenderSecrets is content with every marker replaced by value(ref), in one
// pass: a value that looks like a marker is not expanded again. It asks value
// for the refs in order of first appearance and fails with the first error,
// naming no value.
func RenderSecrets(content []byte, value func(ref string) ([]byte, error)) ([]byte, error) {
	var pairs []string
	for _, ref := range domain.SecretRefsInOrder(content) {
		v, err := value(ref)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, SecretMarker(ref), string(v))
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(content))), nil
}

// CheckSecrets is an error, wrapping ErrSecretUnavailable, when the secrets of
// f are not what they seem: a {{cf-secret: that opens no valid marker, which
// would otherwise be written as it is, or SecretRefs that are not
// SecretRefs(Content).
func (f File) CheckSecrets() error {
	if domain.HasDanglingSecretMarker(f.Content) {
		return fmt.Errorf("%w: %s has a %s that is not a valid marker", ErrSecretUnavailable, f.Path, domain.SecretMarkerPrefix)
	}
	refs := SecretRefs(f.Content)
	if !slices.Equal(f.SecretRefs, refs) {
		return fmt.Errorf("%w: %s says its secret refs are %q, but its content has %q", ErrSecretUnavailable, f.Path, f.SecretRefs, refs)
	}
	return nil
}
