package domain

import (
	"fmt"
	"strconv"
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

// RunEpoch is how a Run under one lease epoch is named in paths, sessions and
// refs: <run>-e<epoch>. It is the one form; ParseRunEpoch reads it back.
func RunEpoch(run string, epoch uint64) string { return fmt.Sprintf("%s-e%d", run, epoch) }

// ParseRunEpoch reads <run>-e<epoch> as RunEpoch writes it: a run that is not
// empty, and an epoch of at least 1 with no leading zeros.
func ParseRunEpoch(s string) (run string, epoch uint64, ok bool) {
	i := strings.LastIndex(s, "-e")
	if i <= 0 {
		return "", 0, false
	}
	epoch, err := strconv.ParseUint(s[i+2:], 10, 63)
	if err != nil || epoch == 0 || RunEpoch(s[:i], epoch) != s {
		return "", 0, false
	}
	return s[:i], epoch, true
}

const sessionPrefix = "cf-"

// SessionName is the name of the session of a Run under a lease epoch (§6.4):
// derived, never stored apart, so the same Run and epoch name the same session.
func SessionName(run string, epoch uint64) string { return sessionPrefix + RunEpoch(run, epoch) }

// ParseSessionName reads the Run and epoch of a name SessionName wrote.
func ParseSessionName(name string) (run string, epoch uint64, ok bool) {
	rest, found := strings.CutPrefix(name, sessionPrefix)
	if !found {
		return "", 0, false
	}
	return ParseRunEpoch(rest)
}

// orphanRefPrefix is where the relay keeps the results of orphans.
const orphanRefPrefix = "refs/cf/orphans/"

// OrphanRef is the ref of the relay that keeps the result of an orphan (§10.4
// point 5): the Run and the lease epoch it ran under. It is only ever created.
func OrphanRef(run ID, epoch uint64) string { return orphanRefPrefix + RunEpoch(string(run), epoch) }

// ValidOrphanRef reports whether ref is the ref of an orphan: a Run ID and an
// epoch, in the one form OrphanRef writes.
func ValidOrphanRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, orphanRefPrefix)
	if !ok {
		return false
	}
	run, _, ok := ParseRunEpoch(rest)
	if !ok {
		return false
	}
	kind, _, err := ParseID(run)
	return err == nil && kind == KindRun
}
