// Package skills holds the skills of `cf` that other parts of the binary write
// where a harness reads them. The files themselves are in the directories here.
package skills

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
)

//go:embed handto/SKILL.md
var handtoTemplate []byte

const versionVar = "{{cf-version}}"

var versionRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// HandtoInRun is the handto skill for a Claude Code session that runs inside a
// Run of a Worker: the one `cf claude install` writes, except that the Workers
// are listed with `cf handoff --list`, which asks the Worker over the socket of
// the Run, instead of `cf worker list`, which needs the credentials of a client
// that a Run does not have.
func HandtoInRun(version string) ([]byte, error) {
	if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("version %q cannot be written into the skill", version)
	}
	out := bytes.ReplaceAll(handtoTemplate, []byte(versionVar), []byte(version))
	return bytes.ReplaceAll(out, []byte("cf worker list"), []byte("cf handoff --list")), nil
}
