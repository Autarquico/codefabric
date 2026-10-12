package redact

import "testing"

func TestText(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // texts to redact, not credentials
		"fatal: unable to access https://u:p@host/r.git: boom": "fatal: unable to access <redacted> boom",
		"see https://host/r?token=1 now":                       "see <redacted> now",
		"plain words stay":                                     "plain words stay",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecretsRemovesTheTokenAnywhere(t *testing.T) {
	token := "abcDEF123456.signature"
	got := Secrets("Authorization: Bearer "+token+" and again x"+token+"y", token)
	if got != "Authorization: Bearer <redacted> and again x<redacted>y" {
		t.Errorf("got %q", got)
	}
	if got := Secrets("short abc", "abc"); got != "short abc" {
		t.Errorf("a short secret shredded the text: %q", got)
	}
}

func TestTailKeepsTheEndAtACharacterBoundary(t *testing.T) {
	if got := Tail("héllo wörld", 6); got != "...wörld" && got != "...örld" {
		t.Errorf("got %q", got)
	}
	if got := Tail("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
}
