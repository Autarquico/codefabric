package svcunit

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

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
