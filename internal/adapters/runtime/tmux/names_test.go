package tmux

import (
	"strings"
	"testing"
)

func TestSessionNameIsDerivedFromTheRunAndTheEpochAndHasNoSeparators(t *testing.T) {
	name := SessionName("run_01J0ABCDEFGHJKMNPQRSTVWXYZ", 3)
	if name != "cf-run_01J0ABCDEFGHJKMNPQRSTVWXYZ-e3" {
		t.Errorf("name = %s", name)
	}
	if strings.ContainsAny(name, ".:") {
		t.Errorf("%s has a character tmux reads as a separator", name)
	}
	if SessionName("run_a", 1) == SessionName("run_a", 2) {
		t.Error("two epochs of a Run share a session")
	}
}

func TestSocketNameIsStablePerDirectoryAndShort(t *testing.T) {
	a, b := SocketName("/data/a"), SocketName("/data/b")
	if a == b || a != SocketName("/data/a") {
		t.Errorf("a=%s b=%s", a, b)
	}
	if !validSocket.MatchString(a) || len(a) != len("cf-")+12 {
		t.Errorf("socket name %q", a)
	}
}

func TestRefsRoundTripAndRefuseWhatIsNotOurs(t *testing.T) {
	r := ref("cf-0123456789ab", "cf-run_x-e1")
	if socket, session, ok := parseRef(r); !ok || socket != "cf-0123456789ab" || session != "cf-run_x-e1" {
		t.Errorf("parseRef(%q) = %q %q %v", r, socket, session, ok)
	}
	for _, bad := range []string{"", "nosep", "a/b/c", "a b/c", "x/..", "x/a;rm", "/s", "s/", "a/=b"} {
		if _, _, ok := parseRef(bad); ok {
			t.Errorf("parseRef(%q) accepted", bad)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][2]int{
		"tmux 3.4": {3, 4}, "tmux 3.3a\n": {3, 3}, "tmux next-3.5": {3, 5}, "tmux 2.9a": {2, 9}, "tmux 3.7c": {3, 7},
	} {
		if major, minor, err := parseVersion(in); err != nil || major != want[0] || minor != want[1] {
			t.Errorf("parseVersion(%q) = %d.%d, %v", in, major, minor, err)
		}
	}
	for _, bad := range []string{"", "tmux", "tmux master"} {
		if _, _, err := parseVersion(bad); err == nil {
			t.Errorf("parseVersion(%q) accepted", bad)
		}
	}
}

func TestNewRefusesASocketThatIsNotAName(t *testing.T) {
	for _, bad := range []string{"", "a/b", "a b", "../x"} {
		if _, err := New(Options{Socket: bad}); err == nil {
			t.Errorf("socket %q accepted", bad)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/tmp/it's here/out.log"); got != `'/tmp/it'\''s here/out.log'` {
		t.Errorf("shellQuote = %s", got)
	}
}
