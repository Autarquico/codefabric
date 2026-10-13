package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
)

func TestLoadClient(t *testing.T) {
	p := config.Paths{ConfigDir: t.TempDir()}
	if c, err := config.LoadClient(p); err != nil || len(c.Attach.SSHTargets) != 0 {
		t.Fatalf("no file: %+v, %v", c, err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.ConfigDir, "client.json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"attach":{"ssh_targets":{"proxmox":"root@10.0.0.5:2222"}}}`)
	if c, err := config.LoadClient(p); err != nil || c.Attach.SSHTargets["proxmox"] != "root@10.0.0.5:2222" {
		t.Fatalf("good file: %+v, %v", c, err)
	}
	for _, bad := range []string{`{"attach":{"ssh_targets":{"w":"-oProxyCommand=x"}}}`, `{"attach":{"ssh_targets":{"w":"a b"}}}`, `{"nope":1}`, `not json`} {
		write(bad)
		if _, err := config.LoadClient(p); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
