package command_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/harness/harnesstest"
)

// The credential of the fixture: command reads no configuration, so no
// snapshot, Launch or Run configuration of it may hold it.
const sentinel = "harnesstest-sentinel-credential"

func TestScenario_HarnessAdapterFramework_Contrato_Command(t *testing.T) {
	harnesstest.Run(t, harnesstest.Fixture{
		New:     func(string) harness.Adapter { return command.Adapter{} },
		Install: installShell,
		SeedHome: func(t *testing.T, home string) {
			write(t, filepath.Join(home, ".shrc"), "export TOKEN="+harness.SecretMarker("shell/token")+"\n")
			write(t, filepath.Join(home, ".credentials"), sentinel+"\n")
		},
		Secrets:         []string{sentinel},
		CredentialFiles: []string{".credentials"},
		Mutate: func(t *testing.T, home string) string {
			write(t, filepath.Join(home, ".shrc"), "export EDITED=1\n")
			return "shrc"
		},
		PrepareRun: func(_ *testing.T, run *harness.RunSpec, hold bool) {
			run.Prompt = "echo command-smoke; exit 3"
			if hold {
				run.Prompt = "echo command-held; while :; do sleep 0.05; done"
			}
		},
		ExpectExit:   3,
		ExpectOutput: "command-smoke",
		Recorded: func(t *testing.T, h harness.Handle) {
			write(t, h.OutputPath, "command-smoke\n")
		},
	})
}

// installShell puts the shell, and the sleep a held Run waits with, in
// binDir: the PATH of the host of a check has nothing else.
func installShell(t *testing.T, binDir string) {
	t.Helper()
	for _, name := range []string{"sh", "sleep"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
