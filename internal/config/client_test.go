package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codeflock/internal/config"
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
	write(`{"notify":{"events":["RunCompleted"],"muted_projects":["p1"],"min_level":"warn","quiet_hours":{"start":"22:00","end":"07:30"},"hook":["/bin/true","x"]}}`)
	if c, err = config.LoadClient(p); err != nil || c.Notify.NotifyEvents()[0] != "RunCompleted" || c.Notify.QuietHours.End != "07:30" || c.Notify.MinLevel != "warn" {
		t.Fatalf("set: %+v, %v", c.Notify, err)
	}
	write(`{"notify":{"events":[]}}`)
	if c, err = config.LoadClient(p); err != nil || len(c.Notify.NotifyEvents()) != 0 {
		t.Fatalf("explicitly none: %+v, %v", c.Notify, err)
	}
	for _, bad := range []string{`{"notify":{"min_level":"critical"}}`, `{"notify":{"quiet_hours":{"start":"25:00","end":"07:00"}}}`, `{"notify":{"hook":[]}}`, `{"notify":{"hook":[""]}}`, `{"notify":{"hook":["./notify.sh"]}}`, `{"notify":{"hook":["bin/hook"]}}`} {
		write(bad)
		if _, err := config.LoadClient(p); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

func TestClientAttach_DetachKey(t *testing.T) {
	p := config.Paths{ConfigDir: t.TempDir()}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.ConfigDir, "client.json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Without a file or a key, the default: Ctrl-].
	c, err := config.LoadClient(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := c.Attach.DetachByte(); err != nil || b != 0x1d {
		t.Fatalf("default = %#x, %v, want Ctrl-] (0x1d)", b, err)
	}
	for key, want := range map[string]byte{"ctrl+]": 0x1d, "ctrl+g": 0x07, "CTRL+G": 0x07, "ctrl+\\": 0x1c, "ctrl+^": 0x1e, "ctrl+_": 0x1f, "ctrl+a": 0x01} {
		raw, _ := json.Marshal(map[string]any{"attach": map[string]string{"detach_key": key}})
		write(string(raw))
		c, err := config.LoadClient(p)
		if err != nil {
			t.Fatalf("%q: %v", key, err)
		}
		if got, err := c.Attach.DetachByte(); err != nil || got != want {
			t.Errorf("%q = %#x, %v, want %#x", key, got, err, want)
		}
	}
	// Keys that would break the session, or that are not a key at all.
	for _, bad := range []string{"ctrl+c", "ctrl+d", "ctrl+z", "ctrl+m", "ctrl+i", "q", "alt+x", "ctrl+", "ctrl+ab", "f5"} {
		raw, _ := json.Marshal(map[string]any{"attach": map[string]string{"detach_key": bad}})
		write(string(raw))
		c, err := config.LoadClient(p)
		if err != nil {
			t.Errorf("detach_key %q stops everything that loads the client config: %v", bad, err)
			continue
		}
		if _, err := c.Attach.DetachByte(); err == nil {
			t.Errorf("detach_key %q was accepted", bad)
		}
		if label := c.Attach.DetachLabel(); label != "" {
			t.Errorf("detach_key %q is labelled %q", bad, label)
		}
	}
}

func TestClientAttach_DetachLabel(t *testing.T) {
	for key, want := range map[string]string{"": "Ctrl-]", "ctrl+]": "Ctrl-]", "ctrl+g": "Ctrl-G", "CTRL+G": "Ctrl-G", "ctrl+\\": "Ctrl-\\"} {
		if got := (config.ClientAttach{DetachKey: key}).DetachLabel(); got != want {
			t.Errorf("%q is labelled %q, want %q", key, got, want)
		}
	}
}
