package svcunit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/svcunit"
)

func TestServicePATH_ProgramsFirstThenTheUsualOnesOnce(t *testing.T) {
	if got := svcunit.ServicePATH("linux", "/opt/cf/bin", "/usr/bin", "relative", "/opt/cf/bin/"); got != "/opt/cf/bin:/usr/bin:/usr/local/bin:/bin" {
		t.Errorf("linux: %s", got)
	}
	if got := svcunit.ServicePATH("darwin"); got != "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("darwin: %s", got)
	}
}

func TestValidatePATH(t *testing.T) {
	for path, valid := range map[string]bool{
		"/usr/bin:/bin": true, "/a b/bin": true,
		"": false, "/usr/bin:": false, "bin:/usr/bin": false, "/usr/bin::/bin": false, "/usr/bin\n": false,
	} {
		if err := svcunit.ValidatePATH(path); (err == nil) != valid {
			t.Errorf("%q: %v", path, err)
		}
	}
}

func TestExecutableWarning(t *testing.T) {
	for program, warns := range map[string]bool{
		"/usr/local/bin/cf":                      false,
		filepath.Join(os.TempDir(), "x", "cf"):   true,
		"/Users/me/Library/Caches/go-build/a/cf": true,
		"/opt/homebrew/Cellar/cf/0.1/bin/cf":     true,
	} {
		if got := svcunit.ExecutableWarning(program); (got != "") != warns {
			t.Errorf("%s: %q", program, got)
		}
	}
}
