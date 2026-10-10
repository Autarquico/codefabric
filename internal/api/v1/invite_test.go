package v1

import (
	"errors"
	"strings"
	"testing"
)

func TestInviteRoundTrip(t *testing.T) {
	want := Invite{Host: "10.0.0.5", Port: "7443", Token: "abc-_123", Fingerprint: strings.Repeat("ab", 32)}
	got, err := ParseInvite(want.String())
	if err != nil || got != want {
		t.Fatalf("ParseInvite(%q) = %+v, %v", want.String(), got, err)
	}
	v6 := Invite{Host: "::1", Port: "7443", Token: "t", Fingerprint: want.Fingerprint}
	if got, err := ParseInvite(v6.String()); err != nil || got != v6 {
		t.Fatalf("ipv6: %+v, %v", got, err)
	}
}

func TestParseInviteRejects(t *testing.T) {
	fp := "#sha256:" + strings.Repeat("00", 32)
	for name, in := range map[string]string{
		"scheme":      "https://h:1/join?token=t" + fp,
		"path":        "cf://h:1/other?token=t" + fp,
		"no port":     "cf://h/join?token=t" + fp,
		"no token":    "cf://h:1/join" + fp,
		"no pin":      "cf://h:1/join?token=t",
		"short pin":   "cf://h:1/join?token=t#sha256:abcd",
		"wrong algo":  "cf://h:1/join?token=t#md5:" + strings.Repeat("00", 32),
		"not a url":   "%%%",
		"port range":  "cf://h:99999/join?token=t" + fp,
		"empty input": "",
	} {
		if _, err := ParseInvite(in); !errors.Is(err, ErrInvalidInvite) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestRedactHidesToken(t *testing.T) {
	inv := Invite{Host: "h", Port: "1", Token: "secret", Fingerprint: strings.Repeat("00", 32)}
	if strings.Contains(inv.Redact(), "secret") {
		t.Fatal(inv.Redact())
	}
}
