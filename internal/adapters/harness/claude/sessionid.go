package claude

import (
	"crypto/sha1" //nolint:gosec // UUID v5 is defined over SHA-1; it is an identifier, not a secret
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/Autarquico/codefabric/internal/domain"
)

// sessionNamespace is the UUID v5 namespace of the sessions CodeFabric starts.
// It never changes: the id of a Run's session is derived from it, and a Start
// repeated after a restart must derive the same one.
var sessionNamespace = [16]byte{
	0x3b, 0x9a, 0x61, 0x0c, 0x7d, 0x42, 0x4e, 0x58,
	0x9f, 0x1e, 0xc4, 0x0d, 0xe5, 0xfa, 0xb2, 0x71,
}

// SessionID is the id of the Claude Code session of a Run in a lease epoch: a
// UUID v5 of "<run>:<epoch>" in sessionNamespace. Start passes it as
// --session-id, so the Worker can know it without reading the stream.
func SessionID(runID domain.ID, epoch uint64) string {
	h := sha1.New() //nolint:gosec // see the import
	h.Write(sessionNamespace[:])
	h.Write([]byte(string(runID) + ":" + strconv.FormatUint(epoch, 10)))
	u := h.Sum(nil)[:16]
	u[6] = u[6]&0x0f | 0x50
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// sessionExists tells whether Claude Code already wrote the session id for the
// cwd, under configDir: Claude Code refuses --session-id for it.
func sessionExists(configDir, cwd, id string) (bool, error) {
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	_, err := os.Stat(filepath.Join(configDir, "projects", encodeProjectDir(cwd), id+".jsonl"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("claude-code: looking for the session %s: %w", id, err)
}

// encodeProjectDir is the name Claude Code gives the directory of a cwd under
// projects/: every character that is not an ASCII letter or digit becomes '-'
// (one per UTF-16 unit, as the harness counts them).
func encodeProjectDir(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteString(strings.Repeat("-", utf16.RuneLen(r)))
	}
	return b.String()
}
