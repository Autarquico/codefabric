// Package skills holds the skills of `cflk` that other parts of the binary write
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

const versionVar = "{{cflk-version}}"

var versionRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// HandtoInRun is the handto skill for a Claude Code session that runs inside a
// Run of a Worker: the one `cflk claude install` writes, except that the Workers
// are listed with `cflk handoff --list`, which asks the Worker over the socket of
// the Run, instead of `cflk worker list`, which needs the credentials of a client
// that a Run does not have.
func HandtoInRun(version string) ([]byte, error) {
	if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("version %q cannot be written into the skill", version)
	}
	out := bytes.ReplaceAll(handtoTemplate, []byte(versionVar), []byte(version))
	// `cflk handoff --list` takes no --state: it lists the Workers a Run may move to.
	out = bytes.ReplaceAll(out, []byte("cflk worker list --state online,degraded"), []byte("cflk handoff --list"))
	return bytes.ReplaceAll(out, []byte("cflk worker list"), []byte("cflk handoff --list")), nil
}
