package domain

import (
	"regexp"
	"strconv"
)

// semver is semver.org 2.0.0 with an optional leading v: no leading zeros in
// the numbers, and a pre-release and build metadata that are well formed.
var semver = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(?:0|[1-9]\d*)` +
	`(?:-(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*)?` +
	`(?:\+[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*)?$`)

type version struct{ major, minor int }

func parseVersion(text string) (version, bool) {
	m := semver.FindStringSubmatch(text)
	if m == nil {
		return version{}, false
	}
	major, errMajor := strconv.Atoi(m[1])
	minor, errMinor := strconv.Atoi(m[2])
	return version{major, minor}, errMajor == nil && errMinor == nil
}

// MinWorkerVersion is the oldest Worker any Controller assigns to: v0.2 is the
// first minor whose Worker carries out stop orders and reconciles (§10.7,
// §27.3, #13). A Worker before it is incompatible whatever the policy of the
// minor before says, since it would block on the orders it does not know.
var MinWorkerVersion = version{0, 2}

// WorkerVersionCompatible is the upgrade policy of §41: a Controller supports
// Workers of its own minor version and of the one before it, within the same
// major. Patch levels and pre-release suffixes do not matter.
//
// If either version is not semver ("dev", a commit hash, empty) it cannot be
// placed on that line, so the two are compatible only if they are the same
// string: two builds of one development version are assumed to match, and
// nothing else is.
func WorkerVersionCompatible(controller, worker string) bool {
	w, workerParses := parseVersion(worker)
	c, controllerParses := parseVersion(controller)
	if !workerParses || !controllerParses {
		return controller == worker
	}
	if w.major < MinWorkerVersion.major || (w.major == MinWorkerVersion.major && w.minor < MinWorkerVersion.minor) {
		return false
	}
	return w.major == c.major && (w.minor == c.minor || w.minor == c.minor-1)
}
