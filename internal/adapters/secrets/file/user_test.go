package file

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
)

func userOf(t *testing.T, s *Store, ref string) string {
	t.Helper()
	metas, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Ref == ref {
			return m.User
		}
	}
	t.Fatalf("no secret %q", ref)
	return ""
}

func TestTheUserOfASecretIsKeptInTheCatalogForBothBackends(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := newKeyringStore(t)
	for _, backend := range []string{secrets.BackendFile, secrets.BackendKeyring} {
		ref := "git/" + backend
		if err := s.Set(ctx, ref, sentinel, nil, backend, secrets.WithUser("deploy-bot")); err != nil {
			t.Fatal(err)
		}
		if got := userOf(t, s, ref); got != "deploy-bot" {
			t.Errorf("%s: user = %q", backend, got)
		}
		got, err := s.Get(ctx, ref)
		if err != nil || got.User != "deploy-bot" {
			t.Errorf("%s: Get = %+v, %v", backend, got.Metadata, err)
		}
	}
}

func TestASecretWithoutAUserHasNone_TheConsumerAppliesTheDefault(t *testing.T) {
	s, _, _ := newStore(t)
	mustSet(t, s, "git/token", sentinel)
	if got := userOf(t, s, "git/token"); got != "" {
		t.Fatalf("user = %q, want none", got)
	}
}

func TestSetReplacesTheUserToo(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.Set(context.Background(), "git/token", sentinel, nil, secrets.BackendFile, secrets.WithUser("alice")); err != nil {
		t.Fatal(err)
	}
	mustSet(t, s, "git/token", sentinel)
	if got := userOf(t, s, "git/token"); got != "" {
		t.Fatalf("user = %q after a Set without one", got)
	}
}

func TestAUserWithCRLFNULOrAColonIsRefusedAndNothingIsStored(t *testing.T) {
	s, _, _ := newStore(t)
	for _, user := range []string{"a\rb", "a\nb", "a\x00b", "a:b", ":", strings.Repeat("u", secrets.MaxUserLen+1)} {
		err := s.Set(context.Background(), "git/token", sentinel, nil, secrets.BackendFile, secrets.WithUser(user))
		if !errors.Is(err, domain.ErrInvalidSecret) {
			t.Errorf("user %q: %v, want ErrInvalidSecret", user, err)
		}
		assertNoValue(t, err.Error())
	}
	if metas, err := s.List(context.Background()); err != nil || len(metas) != 0 {
		t.Fatalf("List = %v, %v after refused Sets", metas, err)
	}
}

func TestACatalogWithAnInvalidUserIsCorrupt(t *testing.T) {
	s, _, path := newStore(t)
	ok := `"created_at":"2026-10-16T12:00:00Z","updated_at":"2026-10-16T12:00:00Z"`
	writeCatalog(t, path, `{"schema":1,"secrets":{"tok":{"backend":"file","value":"czNudGluZWwtVkFMVUU=","user":"a:b",`+ok+`}}}`)
	if _, err := s.List(context.Background()); !errors.Is(err, secrets.ErrCorrupt) {
		t.Fatalf("List = %v, want ErrCorrupt", err)
	}
}
