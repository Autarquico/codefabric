package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/support/migrate"
)

func TestScenario_RenameCLI_ClienteConLaConfigAntigua(t *testing.T) {
	_, invite := bringUpController(t)

	// A client enrolled before the rename: its config is under the old name.
	home := t.TempDir()
	old := config.Paths{
		ConfigDir: filepath.Join(home, ".config", "codefabric"),
		DataDir:   filepath.Join(home, ".local", "share", "codefabric"),
	}
	use(t, old)
	if code, _, errOut := cf(t, invite+"\n", "connect", "-", "--name", "laptop"); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}

	// The first command with the new binary: no variables, the default directories.
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	for _, v := range []string{"CF_CONFIG_DIR", "CF_DATA_DIR", "CFLK_CONFIG_DIR", "CFLK_DATA_DIR"} {
		t.Setenv(v, "")
	}

	var said bytes.Buffer
	release, err := migrate.Client(migrate.ClientEnv{}, &said)
	defer release()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said.String(), "moved "+old.ConfigDir) {
		t.Errorf("the move was not reported: %q", said.String())
	}
	if _, err := os.Stat(old.ConfigDir); !os.IsNotExist(err) {
		t.Errorf("the old config directory is still there: %v", err)
	}
	if code, out, errOut := cf(t, "", "client", "list"); code != 0 || !strings.Contains(out, "laptop") {
		t.Fatalf("client list after the move: exit %d\n%s%s", code, out, errOut)
	}
	said.Reset()
	if _, err := migrate.Client(migrate.ClientEnv{}, &said); err != nil || said.Len() != 0 {
		t.Errorf("a second run said %q (%v); it must be silent", said.String(), err)
	}
}

func TestScenario_RenameCLI_ClienteEnrolado_ConUnaUnidadVieja(t *testing.T) {
	_, invite := bringUpController(t)
	home := t.TempDir()
	old := config.Paths{
		ConfigDir: filepath.Join(home, ".config", "codefabric"),
		DataDir:   filepath.Join(home, ".local", "share", "codefabric"),
	}
	use(t, old)
	if code, _, errOut := cf(t, invite+"\n", "connect", "-", "--name", "laptop"); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	unit := "cf-worker.service"
	if runtime.GOOS == "darwin" {
		unitDir, unit = filepath.Join(home, "Library", "LaunchAgents"), "com.codefabric.worker.plist"
	}
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, unit), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	for _, v := range []string{"CF_CONFIG_DIR", "CF_DATA_DIR", "CFLK_CONFIG_DIR", "CFLK_DATA_DIR"} {
		t.Setenv(v, "")
	}

	if _, err := migrate.Client(migrate.ClientEnv{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old.ConfigDir); err != nil {
		t.Fatalf("the config moved from under a service: %v", err)
	}
	// Still enrolled: the client reads the old directories the unit still uses.
	if code, out, errOut := cf(t, "", "client", "list"); code != 0 || !strings.Contains(out, "laptop") {
		t.Fatalf("client list: exit %d\n%s%s", code, out, errOut)
	}
}
