package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPInstall_DryRunSaysAForeignSkillIsKept(t *testing.T) {
	m := newMCPMachine(t)
	writeLegacy(t, filepath.Dir(m.skill("claude")), "SKILL.md", "mine\n")
	code, out, errOut := run(t, "mcp", "install", "--claude", "--dry-run")
	if code != 0 || strings.Contains(out, "would write") || !strings.Contains(out, ": kept") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
}

func TestMCPRegistrationWhoseExecutableIsGone_IsReplacedOnInstallAndRemovedOnUninstall(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		for _, name := range []string{"cflk", "cf"} {
			t.Run(tool+"/"+name, func(t *testing.T) {
				m := newMCPMachine(t)
				gone := filepath.Join(t.TempDir(), "old-prefix", name)
				m.seed(t, tool, "user", name, gone, "mcp", "serve", "--profile", "read")
				if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
					t.Fatalf("install: exit %d: %s%s", code, out, errOut)
				}
				got := m.registered(t, tool)
				if _, ok := got["cf"]; ok {
					t.Error("the stale cf is still registered")
				}
				requireEqual(t, got["cflk"].Command, selfBinary(t))
				if name == "cflk" {
					m.seed(t, tool, "user", "cflk", gone, "mcp", "serve", "--profile", "read")
				} else {
					m.seed(t, tool, "user", "cf", gone, "mcp", "serve", "--profile", "read")
				}
				if code, out, errOut := run(t, "mcp", "uninstall", "--"+tool); code != 0 {
					t.Fatalf("uninstall: exit %d: %s%s", code, out, errOut)
				}
				requireEqual(t, len(m.registered(t, tool)), 0)
			})
		}
	}
}

func TestMCPInstall_ARelativeCommandNotInPATHIsLeftAlone(t *testing.T) {
	m := newMCPMachine(t)
	m.seed(t, "claude", "user", "cf", "cf", "mcp", "serve", "--profile", "read")
	if code, out, errOut := run(t, "mcp", "install", "--claude"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	requireEqual(t, m.registered(t, "claude")["cf"].Command, "cf")
}

func TestMCPInstall_AFailingCLIDoesNotEchoTheEnvValues(t *testing.T) {
	m := newMCPMachine(t)
	m.seedEnv(t, "claude", "user", "cf", []string{"TOKEN=s3cret"}, betaCf(t), "mcp", "serve", "--profile", "read")
	failing := "#!/bin/sh\ncase \"$2\" in add) echo \"rejected: $*\" >&2; exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(m.bin, "claude"), []byte(failing), 0o755); err != nil { //nolint:gosec // a test double that must run
		t.Fatal(err)
	}
	code, out, errOut := run(t, "mcp", "install", "--claude")
	if code == 0 || strings.Contains(out+errOut, "s3cret") || !strings.Contains(errOut, "TOKEN=***") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
}

func TestMCPInstall_ACfWithEnvThatCflkLacksIsLeftRegistered(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			m.seedEnv(t, tool, "user", "cflk", []string{"A=1", "B=old"}, selfBinary(t), "mcp", "serve", "--profile", "read")
			m.seedEnv(t, tool, "user", "cf", []string{"A=1", "B=new", "C=3"}, betaCf(t), "mcp", "serve", "--profile", "read")
			code, out, errOut := run(t, "mcp", "install", "--"+tool)
			if code != 0 || !strings.Contains(out, "B, C") || strings.Contains(out+errOut, "B=old") {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			got := m.registered(t, tool)
			requireEqual(t, got["cf"].Env["C"], "3")
			requireEqual(t, got["cflk"].Env["B"], "old")
		})
	}
}

func TestMCPInstall_ACflkWithSettingsAddCannotWriteIsNotReplaced(t *testing.T) {
	cases := map[string]string{
		"cwd":     "cwd = \"/work\"\n",
		"timeout": "tool_timeout_sec = 30\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMCPMachine(t)
			cfg := "[mcp_servers.cflk]\ncommand = \"" + selfBinary(t) + "\"\nargs = [\"mcp\", \"serve\", \"--profile\", \"read\"]\n" + extra
			if err := os.WriteFile(m.codexConfig(), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := run(t, "mcp", "install", "--codex")
			if code != 0 || !strings.Contains(out, "left as it is") {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			requireEqual(t, readFile(t, m.codexConfig()), cfg)
			for _, c := range m.calls(t) {
				t.Errorf("codex was run: %s", c)
			}
		})
	}
}

func TestMCPInstall_APutBackThatDoesNotMatchSaysWhatIsLeft(t *testing.T) {
	m := newMCPMachine(t)
	m.seedEnv(t, "claude", "user", "cflk", []string{"K=v"}, selfBinary(t), "mcp", "serve", "--profile", "read")
	claudePath := filepath.Join(m.bin, "claude")
	if err := os.Rename(claudePath, claudePath+"-claudePath"); err != nil {
		t.Fatal(err)
	}
	// the first add fails; the next ones succeed but lose the --env flags
	script := `#!/bin/sh
if [ "$2" = add ]; then
	if [ ! -e "$FAKE_STATE/faulted" ]; then : > "$FAKE_STATE/faulted"; echo bad >&2; exit 1; fi
	n=$#; i=0
	while [ $i -lt $n ]; do a="$1"; shift; i=$((i+1)); if [ "$a" = --env ]; then shift; i=$((i+1)); continue; fi; set -- "$@" "$a"; done
fi
exec /bin/sh "` + claudePath + `-claudePath" "$@"
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil { //nolint:gosec // a test double that must run
		t.Fatal(err)
	}
	code, _, errOut := run(t, "mcp", "install", "--claude")
	if code == 0 || strings.Contains(errOut, "was put back)") || !strings.Contains(errOut, "NOT put back as it was") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestMCPInstall_ACflkLeftAsItIsDoesNotRetireTheCf(t *testing.T) {
	m := newMCPMachine(t)
	self := selfBinary(t)
	cfg := "[mcp_servers.cf]\ncommand = \"" + self + "\"\nargs = [\"mcp\", \"serve\", \"--profile\", \"read\"]\n\n" +
		"[mcp_servers.cflk]\ncommand = \"" + self + "\"\nargs = [\"mcp\", \"serve\", \"--profile\", \"read\"]\nenabled = false\n"
	if err := os.WriteFile(m.codexConfig(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "mcp", "install", "--codex")
	if code != 0 || !strings.Contains(out, "left registered too") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	requireEqual(t, readFile(t, m.codexConfig()), cfg)
}
