package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSSHTarget_Accepts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want SSHTarget
		dest string
	}{
		{"mini", SSHTarget{Host: "mini"}, "mini"},
		{"alex@mini.local", SSHTarget{User: "alex", Host: "mini.local"}, "alex@mini.local"},
		{"alex@mini:2222", SSHTarget{User: "alex", Host: "mini", Port: 2222}, "alex@mini"},
		{"a_b-c@10.0.0.7:22", SSHTarget{User: "a_b-c", Host: "10.0.0.7", Port: 22}, "a_b-c@10.0.0.7"},
		{"u@[fe80::1]", SSHTarget{User: "u", Host: "fe80::1"}, "u@[fe80::1]"},
		{"u@[2001:DB8::1]:65535", SSHTarget{User: "u", Host: "2001:db8::1", Port: 65535}, "u@[2001:db8::1]"},
		{"[::1]", SSHTarget{Host: "::1"}, "[::1]"},
	} {
		got, err := ParseSSHTarget(tc.in)
		if err != nil || got != tc.want || got.Destination() != tc.dest {
			t.Errorf("ParseSSHTarget(%q) = %+v (%q), %v; want %+v (%q)", tc.in, got, got.Destination(), err, tc.want, tc.dest)
		}
	}
}

func TestParseSSHTarget_RejectsWhatCouldBeReadAsSomethingElse(t *testing.T) {
	for _, in := range []string{
		"", "-oProxyCommand=x", "u@-host", "-u@host", ".host", "u@.host", "host:0", "host:65536", "host:-1", "host:+22", "host:022",
		"host:22:22", "u@@host", "u@host name", "u@host;id", "u@host'x", "u@$(id)", "u@host\n", "u@ho`id`st", "u@host|x", "u@host&x",
		"u@[fe80::1%eth0]", "u@[fe80::1%25eth0]:22", "u@[1.2.3.4]", "u@[::1", "u@[::1]x", "u@[::1]:", "u@[zz]", "fe80::1", "u@fe80::1",
		"u@host:", "@host", "u@", "ünï@host", "u@höst", strings.Repeat("a", 256),
	} {
		if got, err := ParseSSHTarget(in); err == nil || !errors.Is(err, ErrMalformedSSHTarget) {
			t.Errorf("ParseSSHTarget(%q) = %+v, %v; want ErrMalformedSSHTarget", in, got, err)
		}
		if ValidSSHTarget(in) {
			t.Errorf("ValidSSHTarget(%q) = true", in)
		}
	}
}

func TestValidAttachBinary(t *testing.T) {
	for _, ok := range []string{"/usr/bin/tmux", "/opt/homebrew/bin/tmux", "/home/a.b/c_d+e-f/tmux"} {
		if !ValidAttachBinary(ok) {
			t.Errorf("ValidAttachBinary(%q) = false", ok)
		}
	}
	for _, bad := range []string{
		"", "/", "tmux", "./tmux", "/usr/../bin/tmux", "/usr/bin/..", "/usr//bin/tmux", "/usr/bin/", "/usr/./bin/tmux",
		"/usr/bin/tm ux", "/usr/bin/tmux;id", "/usr/bin/$(id)", "/usr/bin/tmux'", "/usr/bín/tmux", "/a\n/b", "/" + strings.Repeat("a", 255),
	} {
		if ValidAttachBinary(bad) {
			t.Errorf("ValidAttachBinary(%q) = true", bad)
		}
	}
}

func TestValidAttachBinaries(t *testing.T) {
	if !ValidAttachBinaries(nil) || !ValidAttachBinaries(map[string]string{"tmux": "/usr/bin/tmux"}) {
		t.Error("a nil map and a good entry are valid")
	}
	for name, m := range map[string]map[string]string{
		"bad runtime": {"TMUX": "/usr/bin/tmux"},
		"long name":   {strings.Repeat("a", 33): "/usr/bin/tmux"},
		"bad binary":  {"tmux": "tmux"},
		"too many":    {"a": "/a", "b": "/b", "c": "/c", "d": "/d", "e": "/e"},
	} {
		if ValidAttachBinaries(m) {
			t.Errorf("%s: valid", name)
		}
	}
}
