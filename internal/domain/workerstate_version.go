package domain

import (
	"regexp"
	"strconv"
)

var semver = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$`)

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
	return w.major == c.major && (w.minor == c.minor || w.minor == c.minor-1)
}
