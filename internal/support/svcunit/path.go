package svcunit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// InputError is something the caller asked of a service that it cannot have:
// theirs to fix, under the flag that gave it.
type InputError struct{ Flag, Reason string }

func (e *InputError) Error() string { return e.Flag + ": " + e.Reason }

// Program is the program a service runs: binary, or this executable if binary
// is empty, with a warning if it may not be there later. It refuses a program
// that another user could replace (see replaceable).
func Program(binary string) (program, warning string, err error) {
	program = binary
	if program == "" {
		if program, err = os.Executable(); err != nil {
			return "", "", fmt.Errorf("svcunit: locating this executable: %w", err)
		}
	}
	if !filepath.IsAbs(program) {
		return "", "", &InputError{Flag: "--binary", Reason: "must be an absolute path"}
	}
	if err := replaceable(program, os.Getuid(), trustedGroupOf(runtime.GOOS, os.Getuid())); err != nil {
		return "", "", err
	}
	return program, ExecutableWarning(program), nil
}

// ResolvePATH is the PATH of a service that runs program: given, if it is
// valid, or else one with the directories of program and of the tools found on
// this PATH first.
func ResolvePATH(goos, given, program string, tools ...string) (string, error) {
	if given != "" {
		if err := ValidatePATH(given); err != nil {
			return "", &InputError{Flag: "--path", Reason: err.Error()}
		}
		return given, nil
	}
	dirs := []string{filepath.Dir(program)}
	for _, tool := range tools {
		if path, err := exec.LookPath(tool); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				dirs = append(dirs, filepath.Dir(abs))
			}
		}
	}
	return ServicePATH(goos, dirs...), nil
}

const (
	// maxLinks is how many symbolic links a program may be reached through, as on Linux.
	maxLinks = 40
	xOK      = 1 // X_OK of access(2), which syscall does not name
)

// replaceable refuses a program that is not a file uid can run, or that a user
// other than uid (or root) could swap for one of their own, which the service
// would then run as uid, again and again. It walks the path as the kernel does,
// from the root and down every symbolic link, and refuses it at the first entry
// of another user, or that others or a group that is not trusted (see
// trustedGroup) can write: a directory with the sticky bit, like /tmp, only
// lets them replace their own entries, and what a link says is fixed.
func replaceable(program string, uid int, trusted func(gid int) bool) error {
	refuse := func(format string, a ...any) error {
		return &InputError{Flag: "--binary", Reason: fmt.Sprintf(format, a...)}
	}
	if info, err := os.Stat(program); err != nil {
		return refuse("%s cannot be run: %v", program, errors.Unwrap(err))
	} else if !info.Mode().IsRegular() || syscall.Access(program, xOK) != nil {
		return refuse("%s is not a file you can run", program)
	}
	links := 0
	// resolve follows path from dir, walked already and with no link in it, to the entry it leads to.
	var resolve func(dir, path string) (string, error)
	resolve = func(dir, path string) (string, error) {
		if filepath.IsAbs(path) {
			dir = "/"
		}
		for name := range strings.SplitSeq(path, "/") {
			entry := filepath.Join(dir, name)
			info, err := os.Lstat(entry)
			if err != nil {
				return "", refuse("%s cannot be run: %v", program, err)
			}
			if reason := unsafeTo(info, entry, uid, trusted); reason != "" {
				return "", refuse("%s runs from %s, which %s: another user could replace what the service runs; "+
					"install it where only you and root can write", program, entry, reason)
			}
			if info.Mode()&fs.ModeSymlink == 0 {
				dir = entry
				continue
			}
			if links++; links > maxLinks {
				return "", refuse("%s is reached through more than %d symbolic links", program, maxLinks)
			}
			target, err := os.Readlink(entry)
			if err == nil {
				dir, err = resolve(dir, target)
			}
			if err != nil {
				return "", err
			}
		}
		return dir, nil
	}
	_, err := resolve("/", program)
	return err
}

func unsafeTo(info fs.FileInfo, path string, uid int, trusted func(gid int) bool) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case ok && int(st.Uid) != uid && st.Uid != 0:
		return fmt.Sprintf("belongs to another user (uid %d)", st.Uid)
	case info.Mode()&fs.ModeSymlink != 0, info.IsDir() && info.Mode()&fs.ModeSticky != 0:
		return ""
	case info.Mode().Perm()&0o002 != 0:
		return "anyone can write"
	case ok && info.Mode().Perm()&0o020 != 0 && !trusted(int(st.Gid)):
		return fmt.Sprintf("the members of group %d can write (`chmod g-w %s` stops them)", st.Gid, path)
	}
	return ""
}

// trustedGroupOf says whether a group may write where the services of uid
// run from (see trustedGroup), looking up its name and that of the user.
func trustedGroupOf(goos string, uid int) func(gid int) bool {
	owner, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		owner = nil
	}
	return func(gid int) bool {
		name := ""
		if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
			name = g.Name
		}
		return trustedGroup(goos, gid, name, owner)
	}
}

// trustedGroup says whether the members of a group, writing a program a
// service runs, could do nothing its owner and root could not: a group of
// root's equals (root, wheel, sudo, and admin on macOS), who can replace any
// program anyway, or the private group of the user, of the same name and their
// primary group. An unknown name trusts only root's group.
func trustedGroup(goos string, gid int, name string, owner *user.User) bool {
	switch {
	case gid == 0, name == "wheel", name == "sudo", name == "admin" && goos == "darwin":
		return true
	}
	return owner != nil && name != "" && name == owner.Username && strconv.Itoa(gid) == owner.Gid
}

// ServicePATH is the explicit PATH of a service, calculated when it is
// installed: dirs first (those of the programs the daemon needs), then the
// usual ones of goos. A service manager gives daemons a bare PATH, and the
// shell's would tie the file to the shell it was installed from.
func ServicePATH(goos string, dirs ...string) string {
	usual := []string{"/usr/local/bin", "/usr/bin", "/bin"}
	if goos == "darwin" {
		usual = append([]string{"/opt/homebrew/bin"}, usual...)
	}
	var path []string
	for _, dir := range append(dirs, usual...) {
		if filepath.IsAbs(dir) && !slices.Contains(path, filepath.Clean(dir)) {
			path = append(path, filepath.Clean(dir))
		}
	}
	return strings.Join(path, string(os.PathListSeparator))
}

// ValidatePATH refuses a PATH given by hand that has an empty or a relative
// directory: a service would look for programs where it happens to start.
func ValidatePATH(path string) error {
	for dir := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("the PATH of a service must list absolute directories, and %q is not one", dir)
		}
	}
	return printable(path)
}

// ExecutableWarning says why the program a service runs may not be there
// later, or nothing.
func ExecutableWarning(program string) string {
	switch {
	case strings.HasPrefix(program, filepath.Clean(os.TempDir())+string(filepath.Separator)) || strings.Contains(program, "/go-build"):
		return fmt.Sprintf("the service runs %s, which looks temporary; pass --binary with a lasting path", program)
	case strings.Contains(program, "/Cellar/"):
		return fmt.Sprintf("the service runs %s, which a Homebrew upgrade removes; pass --binary with the path in $(brew --prefix)/bin", program)
	}
	return ""
}
