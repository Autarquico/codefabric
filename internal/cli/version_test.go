package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/buildinfo"
)

func TestScenario_RepoBootstrap_VersionJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	var got buildinfo.Info
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", out.String(), err)
	}
	if got != buildinfo.Get() {
		t.Errorf("got %+v, want %+v", got, buildinfo.Get())
	}
}

func TestVersionHumanOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "cf ") {
		t.Errorf("unexpected output %q", out.String())
	}
}

func TestCompletionIsAvailable(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"completion", "zsh"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "compdef") {
		t.Errorf("zsh completion script not generated")
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code == 0 {
		t.Fatal("expected non-zero exit")
	}
}
