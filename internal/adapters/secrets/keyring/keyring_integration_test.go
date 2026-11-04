//go:build integration && (linux || darwin)

package keyring_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	"github.com/Autarquico/codefabric/internal/adapters/secrets/keyring"
	"github.com/Autarquico/codefabric/internal/adapters/secrets/secretstest"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// requireKeyring skips unless CF_REQUIRE_KEYRING=1: these tests write to the
// OS keyring of the user running them (CI gives them an isolated one).
func requireKeyring(t *testing.T) {
	t.Helper()
	if os.Getenv("CF_REQUIRE_KEYRING") != "1" {
		t.Skip("writes to the OS keyring of this user: CF_REQUIRE_KEYRING=1 runs it")
	}
}

// uniqueAccount is an account no other test or run uses, removed at the end.
func uniqueAccount(t *testing.T, v keyring.Vault) string {
	t.Helper()
	account := "cf-test/" + strings.ReplaceAll(t.Name(), "/", "_") + "/" + filepath.Base(t.TempDir())
	t.Cleanup(func() { deleteLeftover(t, v, account) })
	return account
}

// deleteLeftover removes an item at the end of a test. It cannot use t.Context(),
// which is cancelled before the cleanups run, and it reports an item it could not
// remove: a leak in the keyring of whoever runs the tests.
func deleteLeftover(t *testing.T, v keyring.Vault, account string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := v.Delete(ctx, account); err != nil && !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("removing %q from the keyring: %v", account, err)
	}
}

func TestKeyring_SetGetReplaceAndDeleteOnTheRealKeyring(t *testing.T) {
	requireKeyring(t)
	v := keyring.New()
	account := uniqueAccount(t, v)

	if _, err := v.Get(t.Context(), account); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get of an account never set = %v, want ErrNotFound", err)
	}
	if err := v.Set(t.Context(), account, []byte("first")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := v.Get(t.Context(), account); err != nil || string(got) != "first" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	multiline := "line one\nline \"two\" 'quoted' $HOME é\t"
	if err := v.Set(t.Context(), account, []byte(multiline)); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if got, err := v.Get(t.Context(), account); err != nil || string(got) != multiline {
		t.Fatalf("Get after replace = %q, %v", got, err)
	}
	if err := v.Delete(t.Context(), account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get(t.Context(), account); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := v.Delete(t.Context(), account); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Delete of a missing account = %v, want ErrNotFound", err)
	}
}

func TestKeyring_ALargestValueFitsTheRealKeyring(t *testing.T) {
	requireKeyring(t)
	v := keyring.New()
	account := uniqueAccount(t, v)
	big := strings.Repeat("\"'$\n", keyring.MaxValueLen/4)

	if err := v.Set(t.Context(), account, []byte(big)); err != nil {
		t.Fatalf("Set of %d bytes: %v", len(big), err)
	}
	if got, err := v.Get(t.Context(), account); err != nil || string(got) != big {
		t.Errorf("Get returned %d bytes, %v", len(got), err)
	}
}

// realStore is a file.Store over the real keyring; every item it leaves
// behind is deleted at the end, whatever the test did.
func realStore(t *testing.T) (*file.Store, string, keyring.Vault) {
	t.Helper()
	requireKeyring(t)
	v := keyring.New()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // the catalog directory is 0700, as in production
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets.json")
	s := file.New(path, clock.Real{}, file.WithVault(v))
	t.Cleanup(func() {
		for _, account := range accounts(t, path) {
			deleteLeftover(t, v, account)
		}
	})
	return s, path, v
}

// accounts are the keyring accounts of every item the catalog at path points
// to or has retired: namespace/ref/item.
func accounts(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the catalog of the test
	if err != nil {
		return nil
	}
	var c struct {
		CatalogID string `json:"catalog_id"`
		Secrets   map[string]struct {
			Item string `json:"item"`
		} `json:"secrets"`
		Retired []struct{ Ref, Item string }
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	var out []string
	for ref, e := range c.Secrets {
		if e.Item != "" {
			out = append(out, c.CatalogID+"/"+ref+"/"+e.Item)
		}
	}
	for _, r := range c.Retired {
		out = append(out, c.CatalogID+"/"+r.Ref+"/"+r.Item)
	}
	return out
}

func TestKeyring_StoreKeepsTheValueOnlyInTheRealKeyring(t *testing.T) {
	s, path, _ := realStore(t)
	const value = "ghp_real-keyring-sentinel"
	if err := s.Set(t.Context(), "gh/token", []byte(value), nil, secrets.BackendKeyring); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(t.Context(), "gh/token")
	if err != nil || string(got.Value) != value || got.Backend != secrets.BackendKeyring {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the catalog of the test
	if err != nil {
		t.Fatal(err)
	}
	secretstest.RequireValueOutOfCatalog(t, raw, "gh/token", []byte(value))
	if st, err := s.Check(t.Context()); err != nil || len(st) != 1 || st[0].State != secrets.StateOK {
		t.Errorf("Check = %+v, %v", st, err)
	}
}

func TestKeyring_ReplacingAKeyringSecretWritesANewItemAndDeletesTheOldOne(t *testing.T) {
	s, path, v := realStore(t)
	if err := s.Set(t.Context(), "gh/token", []byte("value-one"), nil, secrets.BackendKeyring); err != nil {
		t.Fatal(err)
	}
	old := accounts(t, path)
	if len(old) != 1 {
		t.Fatalf("accounts after the first Set = %v", old)
	}
	if err := s.Set(t.Context(), "gh/token", []byte("value-two"), nil, secrets.BackendKeyring); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(t.Context(), "gh/token"); err != nil || string(got.Value) != "value-two" {
		t.Fatalf("Get after replace = %q, %v", got.Value, err)
	}
	current := accounts(t, path)
	if len(current) != 1 || current[0] == old[0] {
		t.Fatalf("accounts after replace = %v, was %v: want one, different item", current, old)
	}
	if _, err := v.Get(t.Context(), old[0]); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("the replaced item is still in the keyring: %v", err)
	}
}

func TestKeyring_DeleteRemovesTheItemFromTheRealKeyring(t *testing.T) {
	s, path, v := realStore(t)
	if err := s.Set(t.Context(), "gh/token", []byte("value-one"), nil, secrets.BackendKeyring); err != nil {
		t.Fatal(err)
	}
	account := accounts(t, path)[0]
	if err := s.Delete(t.Context(), "gh/token"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get(t.Context(), account); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("the item is still in the keyring: %v", err)
	}
	if _, err := s.Get(t.Context(), "gh/token"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestKeyring_AnItemRemovedBehindTheCatalogIsBrokenAndDeleteRepairsIt(t *testing.T) {
	s, path, v := realStore(t)
	if err := s.Set(t.Context(), "gh/token", []byte("value-one"), nil, secrets.BackendKeyring); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(t.Context(), "npm/token", []byte("npm-value-1"), nil, secrets.BackendFile); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete(t.Context(), accounts(t, path)[0]); err != nil {
		t.Fatalf("removing the item behind the catalog: %v", err)
	}

	st, err := s.Check(t.Context())
	if err != nil || len(st) != 2 || st[0] != (secrets.Status{Ref: "gh/token", State: secrets.StateBroken}) || st[1].State != secrets.StateOK {
		t.Fatalf("Check = %+v, %v", st, err)
	}
	if _, err := s.Get(t.Context(), "gh/token"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Get of an orphan = %v, want ErrNotFound", err)
	}
	if err := s.Delete(t.Context(), "gh/token"); err != nil {
		t.Fatalf("Delete of an orphan: %v", err)
	}
	if list, err := s.List(t.Context()); err != nil || len(list) != 1 || list[0].Ref != "npm/token" {
		t.Errorf("List after repair = %+v, %v", list, err)
	}
}
