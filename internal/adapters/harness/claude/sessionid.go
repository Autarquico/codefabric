package claude

import (
	"crypto/sha1" //nolint:gosec // UUID v5 is defined over SHA-1; it is an identifier, not a secret
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// sessionIDPattern is the only shape of a session id that reaches a path or an
// argv: a canonical lowercase UUID, which cannot start with '-'.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// maxProjectDirName is the length from which Claude Code shortens the name of
// a project directory and adds a hash (claude-code 2.1.311 and 2.1.319, spike
// addendum case 7).
const maxProjectDirName = 200

// sessionExists tells whether Claude Code already wrote the session id for the
// cwd, under configDir: Claude Code refuses --session-id for it.
func sessionExists(configDir, cwd, id string) (bool, error) {
	if !validSessionID(id) {
		return false, fmt.Errorf("claude-code: %q is not a session id", id)
	}
	_, err := os.Stat(filepath.Join(sessionProjectDir(configDir, cwd, id), id+".jsonl"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("claude-code: looking for the session %s: %w", id, err)
}

// projectDir is the directory of the sessions of a cwd. Like Claude Code, it
// names the cwd with its symlinks resolved when it exists.
func projectDir(configDir, cwd string) string {
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	return filepath.Join(configDir, "projects", encodeProjectDir(cwd))
}

// sessionProjectDir is projectDir, except that for a long cwd, whose hash
// suffix is the part of the rule that is easiest to get wrong, it looks for
// the directory with the same first 200 characters that holds the session.
func sessionProjectDir(configDir, cwd, id string) string {
	dir := projectDir(configDir, cwd)
	base := filepath.Base(dir)
	if len(base) <= maxProjectDirName {
		return dir
	}
	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); err == nil {
		return dir
	}
	prefix := base[:maxProjectDirName] + "-"
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dir), prefix+"*", id+".jsonl"))
	if err != nil || len(matches) == 0 {
		return dir
	}
	slices.Sort(matches)
	return filepath.Dir(matches[0])
}

// encodeProjectDir is the name Claude Code gives the directory of a cwd under
// projects/ (claude-code 2.1.311): every character that is not an ASCII letter
// or digit becomes '-', one per UTF-16 unit. Past 200 characters the name is
// the first 200, '-', and base36(abs(h)) with h the Java-style int32 hash
// (h = 31*h + unit) of the cwd as it is, not encoded.
func encodeProjectDir(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteString(strings.Repeat("-", utf16.RuneLen(r)))
	}
	name := b.String()
	if len(name) <= maxProjectDirName {
		return name
	}
	var h int32
	for _, u := range utf16.Encode([]rune(cwd)) {
		h = 31*h + int32(u)
	}
	abs := int64(h)
	if abs < 0 {
		abs = -abs
	}
	return name[:maxProjectDirName] + "-" + strconv.FormatInt(abs, 36)
}
