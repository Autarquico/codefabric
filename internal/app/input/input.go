// Package input holds the checks use cases apply to what callers send. Each
// failure is a CF_INVALID_REQUEST that names the offending field.
package input

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	maxNameLen = 63
	maxRefLen  = 255
	maxURLLen  = 2048
)

// Invalid is the error for a request that cannot be honoured as sent.
func Invalid(format string, args ...any) *domain.Error {
	return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, args...), nil)
}

var slug = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Name checks a Project or Task name (§3.13): a lower-case slug that no
// reference rule would read as an ID, since IDs resolve before names.
func Name(what, name string) error {
	if len(name) > maxNameLen || !slug.MatchString(name) {
		return Invalid("%s must be a slug of at most %d characters: lower-case letters and digits, joined by '-', '_' or '.'", what, maxNameLen)
	}
	if _, err := domain.ParseShortID(name); err == nil {
		return Invalid("%s %q reads as a short ID", what, name)
	}
	if _, _, err := domain.ParseID(name); err == nil {
		return Invalid("%s %q reads as an ID", what, name)
	}
	return nil
}

// Repo checks a repository URL or path. A leading '-' is refused because git
// would take it for an option.
func Repo(repo string) error {
	if repo == "" || len(repo) > maxURLLen || strings.HasPrefix(repo, "-") || hasSpaceOrControl(repo) {
		return Invalid("repo must be a git URL or path without spaces, at most %d characters", maxURLLen)
	}
	return nil
}

// Ref checks a branch, tag or other git ref loosely; git has the last word.
func Ref(what, ref string) error {
	if ref == "" || len(ref) > maxRefLen || strings.HasPrefix(ref, "-") ||
		strings.Contains(ref, "..") || hasSpaceOrControl(ref) {
		return Invalid("%s must be a git ref of at most %d characters", what, maxRefLen)
	}
	return nil
}

// Label checks a short free-form reference such as a workflow or environment.
func Label(what, s string) error {
	if len(s) > maxRefLen || hasSpaceOrControl(s) {
		return Invalid("%s must have no spaces and at most %d characters", what, maxRefLen)
	}
	return nil
}

// Mode accepts the empty mode, which means inherited.
func Mode(m domain.Mode) error {
	if m != "" && m != domain.ModeHeadless && m != domain.ModeInteractive {
		return Invalid("mode must be headless or interactive")
	}
	return nil
}

// Completion accepts the empty policy, which means inherited.
func Completion(c domain.Completion) error {
	switch c {
	case "", domain.CompletionFirstSuccess, domain.CompletionPromote, domain.CompletionManual:
		return nil
	}
	return Invalid("completion must be first_success, promote or manual")
}

// Resources accepts nil, which means inherited.
func Resources(r *v1.Resources) error {
	if r != nil && (r.CPU <= 0 || r.MemoryMB <= 0) {
		return Invalid("resources need a positive cpu and memory_mb")
	}
	return nil
}

// Retry accepts nil, which means inherited.
func Retry(r *v1.Retry) error {
	if r == nil {
		return nil
	}
	for _, a := range []domain.RetryAction{r.OnFailed, r.OnLost} {
		if a != domain.RetryNone && a != domain.RetryReschedule && a != domain.RetryAsk {
			return Invalid("retry actions must be none, reschedule or ask")
		}
	}
	if r.MaxAttempts < 1 {
		return Invalid("retry max_attempts must be at least 1")
	}
	return nil
}

// Requirements checks what a Task asks of where it runs.
func Requirements(r v1.Requirements) error {
	if err := Resources(r.Resources); err != nil {
		return err
	}
	for what, values := range map[string][]string{
		"capabilities": r.Capabilities, "workers.prefer": r.Workers.Prefer,
		"workers.avoid": r.Workers.Avoid, "workers.require": r.Workers.Require,
	} {
		for _, v := range values {
			if v == "" || len(v) > maxRefLen || hasSpaceOrControl(v) {
				return Invalid("%s entries must be non-empty words of at most %d characters", what, maxRefLen)
			}
		}
	}
	if err := Label("harness", r.Harness); err != nil {
		return err
	}
	return Label("model", r.Model)
}

func hasSpaceOrControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
}

// First returns the first failure of a series of checks.
func First(checks ...error) error {
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	return nil
}
