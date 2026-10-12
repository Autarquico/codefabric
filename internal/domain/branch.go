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

// RunBranch is the branch of a Run: the one frozen when it was created, or, for
// a Run that has none stored, the one BranchOf gives. It is the one rule for
// whoever reads a Run.
func RunBranch(stored, taskName string, task, run ShortID) string {
	if stored != "" {
		return stored
	}
	return BranchOf(taskName, task, run)
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

// orphanRefPrefix is where the relay keeps the results of orphans.
const orphanRefPrefix = "refs/cf/orphans/"

// OrphanRef is the ref of the relay that keeps the result of an orphan (§10.4
// point 5): the Run and the lease epoch it ran under. It is only ever created.
func OrphanRef(run ID, epoch uint64) string {
	return fmt.Sprintf("%s%s-e%d", orphanRefPrefix, run, epoch)
}

// ValidOrphanRef reports whether ref is the ref of an orphan: a Run ID and an
// epoch, in the one form OrphanRef writes.
func ValidOrphanRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, orphanRefPrefix)
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, "-e")
	if i < 0 {
		return false
	}
	kind, _, err := ParseID(rest[:i])
	var epoch uint64
	if err != nil || kind != KindRun || len(rest[i+2:]) == 0 || rest[i+2] == '0' {
		return false
	}
	if _, err := fmt.Sscanf(rest[i+2:], "%d", &epoch); err != nil || epoch == 0 {
		return false
	}
	return OrphanRef(ID(rest[:i]), epoch) == ref
}
