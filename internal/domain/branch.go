package domain

import (
	"fmt"
	"strings"
)

// BranchOf is the branch a Run publishes to (§3.4): cf/<task>/<run>. It is the
// one place that builds it, for the Controller that freezes it and for whoever
// reads a Run that has none stored. A Task name that git would refuse as a
// component of a ref (a name that ends in .lock is one) is replaced by the short
// ID of the Task.
func BranchOf(taskName string, task, run ShortID) string {
	component := taskName
	if !ValidRefComponent(component) {
		component = task.String()
	}
	return fmt.Sprintf("cf/%s/%s", component, run)
}

// ValidRefComponent reports whether s can be one component of a git ref, by the
// rules of git-check-ref-format that matter for the names CodeFabric builds.
func ValidRefComponent(s string) bool {
	switch {
	case s == "", strings.HasPrefix(s, "."), strings.HasSuffix(s, "."), strings.HasSuffix(s, ".lock"):
		return false
	case strings.Contains(s, ".."), strings.Contains(s, "@{"), s == "@":
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\/", r)
	})
}

// ValidBranch reports whether name is a full branch name of CodeFabric's shape:
// cf/<task>/<run>, with every component a valid ref component.
func ValidBranch(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "cf" {
		return false
	}
	for _, p := range parts {
		if !ValidRefComponent(p) {
			return false
		}
	}
	return true
}
