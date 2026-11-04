package daemon

import (
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
)

func userOfRef(t *testing.T, w *keyringWorker, ref string) string {
	t.Helper()
	got, err := w.store.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return got.User
}

func TestSecretsSet_UserIsStoredLocallyAndNeverReported(t *testing.T) {
	w := newKeyringWorker(t)
	out, err := cf(t, keyringValue, "set", "git/origin", "--user", "deploy-bot", "--json")
	if err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	if strings.Contains(out, "deploy-bot") {
		t.Errorf("the user is in the output of set: %s", out)
	}
	if got := userOfRef(t, w, "git/origin"); got != "deploy-bot" {
		t.Errorf("user = %q", got)
	}
	if list, err := cf(t, "", "list", "--json"); err != nil || strings.Contains(list, "deploy-bot") {
		t.Errorf("the user is in the list (%v): %s", err, list)
	}
	if !strings.Contains(strings.Join(w.announced(t), ","), "git/origin") {
		t.Errorf("the secret is not announced: %v", w.announced(t))
	}
}

func TestSecretsSet_WithoutUserTheSecretHasNoneAndAReplacementClearsIt(t *testing.T) {
	w := newKeyringWorker(t)
	if _, err := cf(t, keyringValue, "set", "git/origin"); err != nil {
		t.Fatal(err)
	}
	if got := userOfRef(t, w, "git/origin"); got != "" {
		t.Errorf("user = %q, want none (the consumer applies %s)", got, secrets.DefaultUser)
	}
	if _, err := cf(t, keyringValue, "set", "git/origin", "--user", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := cf(t, keyringValue, "set", "git/origin"); err != nil {
		t.Fatal(err)
	}
	if got := userOfRef(t, w, "git/origin"); got != "" {
		t.Errorf("user = %q after setting it again without --user", got)
	}
}

func TestSecretsSet_UserWithCRLFNULOrAColonIsRefusedBeforeReadingTheValue(t *testing.T) {
	w := newKeyringWorker(t)
	for _, user := range []string{"a\rb", "a\nb", "a\x00b", "a:b"} {
		out, err := cf(t, keyringValue, "set", "git/origin", "--user", user)
		if domainCode(err) != domain.CodeInvalidRequest {
			t.Errorf("user %q: %v", user, err)
		}
		if strings.Contains(out+err.Error(), user) && strings.TrimSpace(user) != "" {
			t.Errorf("the message repeats the user %q: %s %v", user, out, err)
		}
	}
	if _, err := w.store.Get(t.Context(), "git/origin"); err == nil {
		t.Error("a secret was stored with an invalid user")
	}
}
