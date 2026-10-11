package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Name is how handles say that tmux made them.
const Name = "tmux"

// SocketName names the tmux server of a Worker by its data directory, so that
// each Worker, and each test, has a server of its own. It is a name for -L, not
// a path: a socket path of a deep directory passes the 104 bytes macOS allows.
func SocketName(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))
	return "cf-" + hex.EncodeToString(sum[:])[:12]
}

var (
	validSocket  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	validSession = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
)

func ref(socket, session string) string { return socket + "/" + session }

// parseRef splits a handle's ref, refusing anything that is not a name this
// package would have made: a ref is data from a database.
func parseRef(r string) (socket, session string, ok bool) {
	socket, session, found := strings.Cut(r, "/")
	if !found || !validSocket.MatchString(socket) || !validSession.MatchString(session) {
		return "", "", false
	}
	return socket, session, true
}

// parseVersion reads the output of `tmux -V`: "tmux 3.4", "tmux 3.3a",
// "tmux next-3.5".
func parseVersion(out string) (major, minor int, err error) {
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("tmux: cannot read the version from %q", out)
	}
	v := strings.TrimPrefix(fields[len(fields)-1], "next-")
	m := regexp.MustCompile(`^(\d+)\.(\d+)`).FindStringSubmatch(v)
	if m == nil {
		return 0, 0, fmt.Errorf("tmux: cannot read the version from %q", out)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, nil
}
