package domain

import (
	"regexp"
	"strconv"
)

// devVersion is what a binary built without a release reports.
const devVersion = "dev"

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
// A Worker whose version is "dev", empty or not semver cannot be placed on
// that line, so it is compatible only with a Controller that is itself "dev":
// two development builds are assumed to match.
func WorkerVersionCompatible(controller, worker string) bool {
	w, workerParses := parseVersion(worker)
	if !workerParses {
		return controller == devVersion
	}
	c, controllerParses := parseVersion(controller)
	return controllerParses && w.major == c.major && (w.minor == c.minor || w.minor == c.minor-1)
}
