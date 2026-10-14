package svcunit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	if err := replaceable(program, os.Getuid()); err != nil {
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

// replaceable refuses a program that a user other than uid (or root) could
// swap for one of their own, which the service would then run as uid, again
// and again: one whose file or any directory above it, through symbolic links
// or not, is owned by another user, or is writable by others without the
// sticky bit. What does not exist yet is judged by the nearest directory that does.
func replaceable(program string, uid int) error {
	paths := []string{program}
	if resolved, err := filepath.EvalSymlinks(program); err == nil && resolved != program {
		paths = append(paths, resolved)
	} else if target, err := os.Readlink(program); err == nil {
		// A link to nothing yet: whoever can create its target decides what runs.
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(program), target)
		}
		paths = append(paths, target)
	}
	for _, path := range paths {
		for {
			info, err := os.Stat(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return fmt.Errorf("svcunit: %w", err)
			default:
				if reason := unsafeTo(info, uid); reason != "" {
					return &InputError{Flag: "--binary", Reason: fmt.Sprintf("%s runs from %s, which %s: another user could "+
						"replace what the service runs; install it where only you and root can write", program, path, reason)}
				}
			}
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			path = parent
		}
	}
	return nil
}

func unsafeTo(info fs.FileInfo, uid int) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid && st.Uid != 0 {
		return fmt.Sprintf("belongs to another user (uid %d)", st.Uid)
	}
	if info.Mode().Perm()&0o002 != 0 && (!info.IsDir() || info.Mode()&fs.ModeSticky == 0) {
		return "anyone can write"
	}
	return ""
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
