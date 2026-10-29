package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrInvalidWorkflowRef reports text that is not a Workflow reference.
var ErrInvalidWorkflowRef = errors.New("invalid workflow reference")

// MaxSlugLen is the longest slug.
const MaxSlugLen = 63

var slugForm = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// ValidSlug reports whether s is a slug (§3.13): lower-case letters and
// digits joined by '-', '_' or '.', of at most MaxSlugLen characters. It is
// the shape of Project, Task and Workflow names.
func ValidSlug(s string) bool {
	return len(s) <= MaxSlugLen && slugForm.MatchString(s)
}

// ValidWorkflowName reports whether name is a Workflow name.
func ValidWorkflowName(name string) bool { return ValidSlug(name) }

// WorkflowSelector is the parsed text form of a reference; it points at a Workflow, and optionally at one of its versions.
// Exactly one of Latest, Version and Digest is set when a version is named;
// none is set for a bare name, which callers read as "latest".
type WorkflowSelector struct {
	Name    string
	Latest  bool
	Version int
	Digest  string
}

// ParseWorkflowRef accepts `name`, `name@latest`, `name@<N>` (N >= 1, no
// leading zeros) and `name@sha256:<hex>`.
func ParseWorkflowRef(text string) (WorkflowSelector, error) {
	name, version, hasVersion := strings.Cut(text, "@")
	if !ValidWorkflowName(name) {
		return WorkflowSelector{}, fmt.Errorf("%w: %q", ErrInvalidWorkflowRef, text)
	}
	ref := WorkflowSelector{Name: name}
	if !hasVersion {
		return ref, nil
	}
	if version == "latest" {
		ref.Latest = true
		return ref, nil
	}
	if digestForm.MatchString(version) {
		ref.Digest = version
		return ref, nil
	}
	n, err := strconv.Atoi(version)
	if err != nil || n < 1 || strconv.Itoa(n) != version {
		return WorkflowSelector{}, fmt.Errorf("%w: %q", ErrInvalidWorkflowRef, text)
	}
	ref.Version = n
	return ref, nil
}
