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

func TestLoadClient_Notify(t *testing.T) {
	p := config.Paths{ConfigDir: t.TempDir()}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.ConfigDir, "client.json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{}`)
	c, err := config.LoadClient(p)
	if err != nil || len(c.Notify.NotifyEvents()) != 3 {
		t.Fatalf("defaults: %+v, %v", c.Notify.NotifyEvents(), err)
	}
	write(`{"notify":{"events":["RunCompleted"],"muted_projects":["p1"],"quiet_hours":{"start":"22:00","end":"07:30"},"hook":["/bin/true","x"]}}`)
	if c, err = config.LoadClient(p); err != nil || c.Notify.NotifyEvents()[0] != "RunCompleted" || c.Notify.QuietHours.End != "07:30" {
		t.Fatalf("set: %+v, %v", c.Notify, err)
	}
	write(`{"notify":{"events":[]}}`)
	if c, err = config.LoadClient(p); err != nil || len(c.Notify.NotifyEvents()) != 0 {
		t.Fatalf("explicitly none: %+v, %v", c.Notify, err)
	}
	for _, bad := range []string{`{"notify":{"quiet_hours":{"start":"25:00","end":"07:00"}}}`, `{"notify":{"hook":[]}}`, `{"notify":{"hook":[""]}}`} {
		write(bad)
		if _, err := config.LoadClient(p); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
