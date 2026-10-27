package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Limits of the secrets of a Worker and of a Run (§16.7). A ref is exact text:
// there are no globs, and an allowlist names every ref it allows.
const (
	MaxSecretRefLen     = 128
	MaxSecretEnvNameLen = 128

	// A value shorter than MinSecretValueLen cannot be redacted without
	// redacting ordinary text too, so it is refused where a value is registered
	// (`cf worker secrets set`) and never reaches a Run.
	MinSecretValueLen = 8
	MaxSecretValueLen = 64 << 10

	MaxWorkerSecrets  = 256 // refs in the catalog of one Worker
	MaxProjectSecrets = 256 // refs in the allowlist of one Project
	MaxRunSecrets     = 32  // refs one Run may use
	MaxSecretProjects = 8   // Projects a Worker may scope one ref to
)

// ErrInvalidSecret wraps every malformed ref, environment name or value. Its
// messages name refs and environment names, never a value.
var ErrInvalidSecret = errors.New("invalid secret")

// SecretEnv is an environment variable of the harness whose value is the
// secret Ref, resolved on the Worker just before exec.
type SecretEnv struct {
	Name string
	Ref  string
}

// reservedSecretEnvPrefixes and reservedSecretEnv are the environment the
// Worker and the shell own: a secret must not replace them.
var (
	reservedSecretEnvPrefixes = []string{"CF_", "GIT_", "TMUX"}
	reservedSecretEnv         = []string{"PATH", "HOME", "SHELL", "USER"}
)

// ValidSecretRef reports whether ref names a secret: a letter or a digit, then
// letters, digits, '.', '_', '/' and '-', up to MaxSecretRefLen in all.
func ValidSecretRef(ref string) bool {
	if ref == "" || len(ref) > MaxSecretRefLen || !isAlnum(ref[0]) {
		return false
	}
	for i := range len(ref) {
		if c := ref[i]; !isAlnum(c) && !strings.ContainsRune("._/-", rune(c)) {
			return false
		}
	}
	return true
}

// ReservedSecretEnv reports whether name belongs to the Worker or the shell and
// cannot carry a secret.
func ReservedSecretEnv(name string) bool {
	return slices.Contains(reservedSecretEnv, name) ||
		slices.ContainsFunc(reservedSecretEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

// ValidSecretEnvName reports whether name can carry a secret: a shell variable
// name ([A-Za-z_][A-Za-z0-9_]*, up to MaxSecretEnvNameLen) that is not reserved.
func ValidSecretEnvName(name string) bool {
	if name == "" || len(name) > MaxSecretEnvNameLen || isDigit(name[0]) {
		return false
	}
	for i := range len(name) {
		if c := name[i]; !isAlnum(c) && c != '_' {
			return false
		}
	}
	return !ReservedSecretEnv(name)
}

// CheckSecretRefs is an error, wrapping ErrInvalidSecret, unless refs are at
// most limit valid refs, none repeated.
func CheckSecretRefs(refs []string, limit int) error {
	if len(refs) > limit {
		return fmt.Errorf("%w: %d refs, at most %d", ErrInvalidSecret, len(refs), limit)
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		switch {
		case !ValidSecretRef(ref):
			return fmt.Errorf("%w: ref %q is not a letter or digit followed by letters, digits, '.', '_', '/' or '-', up to %d", ErrInvalidSecret, ref, MaxSecretRefLen)
		case seen[ref]:
			return fmt.Errorf("%w: ref %q is repeated", ErrInvalidSecret, ref)
		}
		seen[ref] = true
	}
	return nil
}

// CheckSecretEnv is an error, wrapping ErrInvalidSecret, unless env is at most
// MaxRunSecrets variables, each a valid name and ref, no name repeated. Two
// names may share a ref.
func CheckSecretEnv(env []SecretEnv) error {
	if len(env) > MaxRunSecrets {
		return fmt.Errorf("%w: %d secret variables, at most %d", ErrInvalidSecret, len(env), MaxRunSecrets)
	}
	seen := make(map[string]bool, len(env))
	for _, e := range env {
		switch {
		case !ValidSecretEnvName(e.Name):
			return fmt.Errorf("%w: %q cannot carry a secret: it must match [A-Za-z_][A-Za-z0-9_]*, and neither start with CF_, GIT_ or TMUX nor be PATH, HOME, SHELL or USER", ErrInvalidSecret, e.Name)
		case seen[e.Name]:
			return fmt.Errorf("%w: variable %q is repeated", ErrInvalidSecret, e.Name)
		case !ValidSecretRef(e.Ref):
			return fmt.Errorf("%w: variable %q names ref %q, which is not a valid ref", ErrInvalidSecret, e.Name, e.Ref)
		}
		seen[e.Name] = true
	}
	return nil
}

// SecretRefsOf is the set of refs env uses, sorted.
func SecretRefsOf(env []SecretEnv) []string {
	refs := make([]string, 0, len(env))
	for _, e := range env {
		refs = append(refs, e.Ref)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// CheckSecretValue is an error, wrapping ErrInvalidSecret, unless value is
// between MinSecretValueLen and MaxSecretValueLen bytes. It is the check of
// whoever registers a value; the error says only the size.
func CheckSecretValue(value []byte) error {
	switch {
	case len(value) < MinSecretValueLen:
		return fmt.Errorf("%w: the value has %d bytes; it needs at least %d to be redacted from output", ErrInvalidSecret, len(value), MinSecretValueLen)
	case len(value) > MaxSecretValueLen:
		return fmt.Errorf("%w: the value has %d bytes, at most %d", ErrInvalidSecret, len(value), MaxSecretValueLen)
	}
	return nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlnum(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
