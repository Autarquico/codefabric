package file

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// fakeVault is a keyring in memory.
type fakeVault struct {
	mu    sync.Mutex
	items map[string][]byte
	// locked makes every call ErrUnavailable; failDelete only Delete.
	locked, failDelete bool
	gets               int
	// beforeGet and afterSet, if set, run outside the lock.
	beforeGet func(account string)
	afterSet  func(account string)
}

func newFakeVault() *fakeVault { return &fakeVault{items: map[string][]byte{}} }

func (v *fakeVault) Get(_ context.Context, account string) ([]byte, error) {
	v.mu.Lock()
	hook := v.beforeGet
	v.beforeGet = nil
	v.mu.Unlock()
	if hook != nil {
		hook(account)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.gets++
	if v.locked {
		return nil, secrets.ErrUnavailable
	}
	val, ok := v.items[account]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return bytes.Clone(val), nil
}

func (v *fakeVault) Set(_ context.Context, account string, value []byte) error {
	v.mu.Lock()
	if v.locked {
		v.mu.Unlock()
		return secrets.ErrUnavailable
	}
	v.items[account] = bytes.Clone(value)
	hook := v.afterSet
	v.mu.Unlock()
	if hook != nil {
		hook(account)
	}
	return nil
}

func (v *fakeVault) Delete(_ context.Context, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.locked || v.failDelete {
		return secrets.ErrUnavailable
	}
	if _, ok := v.items[account]; !ok {
		return secrets.ErrNotFound
	}
	delete(v.items, account)
	return nil
}

func (v *fakeVault) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.items)
}

func newKeyringStore(t *testing.T) (*Store, *fakeVault, *clock.Fake, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker", "secrets.json")
	c := clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	v := newFakeVault()
	return New(path, c, WithVault(v)), v, c, path
}

func setKeyring(t *testing.T, s *Store, ref string, value []byte, projects ...string) {
	t.Helper()
	if err := s.Set(context.Background(), ref, value, projects, secrets.BackendKeyring); err != nil {
		t.Fatalf("Set(%q): %v", ref, err)
	}
}

func statusOf(t *testing.T, s *Store, ref string) secrets.State {
	t.Helper()
	statuses, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range statuses {
		if st.Ref == ref {
			return st.State
		}
	}
	t.Fatalf("no status for %q in %v", ref, statuses)
	return ""
}

func TestKeyringKeepsTheValueOutOfTheCatalog(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", sentinel, projectA)

	raw, err := os.ReadFile(path) //nolint:gosec // inside t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, sentinel) {
		t.Fatalf("the catalog holds the value: %s", raw)
	}
	c := readCatalog(t, path)
	e := c.Secrets["gh/token"]
	if e.Backend != secrets.BackendKeyring || c.CatalogID == "" {
		t.Fatalf("entry %+v, catalog id %q", e, c.CatalogID)
	}
	if got := string(v.items[account(c.CatalogID, "gh/token", e.Item)]); got != string(sentinel) {
		t.Fatalf("vault holds %q under the account of the entry", got)
	}
	got, err := s.Get(context.Background(), "gh/token")
	if err != nil || !bytes.Equal(got.Value, sentinel) || got.Backend != secrets.BackendKeyring {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

func TestListNeverAsksTheVault(t *testing.T) {
	s, v, _, _ := newKeyringStore(t)
	setKeyring(t, s, "gh/token", sentinel)
	v.gets = 0
	metas, err := s.List(context.Background())
	if err != nil || len(metas) != 1 || metas[0].Backend != secrets.BackendKeyring || v.gets != 0 {
		t.Fatalf("List = %+v, %v; vault reads %d", metas, err, v.gets)
	}
}

func TestKeyringSetReplacesTheItemAndDeletesTheOldOne(t *testing.T) {
	s, v, c, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"), projectA)
	created := readCatalog(t, path).Secrets["gh/token"].CreatedAt
	c.Advance(time.Minute)
	setKeyring(t, s, "gh/token", []byte("second-value-1234"), projectB)

	cat := readCatalog(t, path)
	if v.count() != 1 || len(cat.Retired) != 0 {
		t.Fatalf("vault holds %d items, %d retired", v.count(), len(cat.Retired))
	}
	got, err := s.Get(context.Background(), "gh/token")
	if err != nil || string(got.Value) != "second-value-1234" || got.Projects[0] != projectB || !got.CreatedAt.Equal(created) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

// A reader that read the catalog before a Set and reaches the vault after it
// finds its item gone: it reads the catalog again and gets the new version,
// scope included, never the old scope with the new value.
func TestGetReadsTheCatalogAgainWhenItsItemWasReplaced(t *testing.T) {
	s, v, _, _ := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"), projectA)
	v.beforeGet = func(string) { setKeyring(t, s, "gh/token", []byte("second-value-1234"), projectB) }

	got, err := s.Get(context.Background(), "gh/token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != "second-value-1234" || len(got.Projects) != 1 || got.Projects[0] != projectB {
		t.Fatalf("Get = %q for %v", got.Value, got.Projects)
	}
}

// While a Set is between its vault write and its catalog swap, readers see the
// old version whole.
func TestReadersSeeTheOldVersionUntilTheSwap(t *testing.T) {
	s, v, _, _ := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"), projectA)
	var during secrets.Secret
	var duringErr error
	v.afterSet = func(string) { during, duringErr = s.Get(context.Background(), "gh/token") }

	setKeyring(t, s, "gh/token", []byte("second-value-1234"), projectB)

	if duringErr != nil || string(during.Value) != "first-value-1234" || during.Projects[0] != projectA {
		t.Fatalf("mid-Set Get = %q %v, %v", during.Value, during.Projects, duringErr)
	}
}

func TestCrashBetweenTheVaultWriteAndTheSwapLeavesTheOldVersionAndASweepableItem(t *testing.T) {
	s, v, c, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"), projectA)
	dir := filepath.Dir(path)
	v.afterSet = func(string) { _ = os.Chmod(dir, 0o500) } //nolint:gosec // test: the swap cannot be written
	err := s.Set(context.Background(), "gh/token", []byte("second-value-1234"), nil, secrets.BackendKeyring)
	v.afterSet = nil
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // test cleanup
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("the swap should have failed")
	}

	got, gerr := s.Get(context.Background(), "gh/token")
	if gerr != nil || string(got.Value) != "first-value-1234" || got.Projects[0] != projectA {
		t.Fatalf("after the crash Get = %q %v, %v", got.Value, got.Projects, gerr)
	}
	cat := readCatalog(t, path)
	if v.count() != 2 || len(cat.Retired) != 1 || !cat.Retired[0].Pending {
		t.Fatalf("vault holds %d items; retired %+v", v.count(), cat.Retired)
	}

	// Within the grace the item may belong to a Set still running.
	setKeyring(t, s, "other", []byte("other-value-1234"))
	if len(readCatalog(t, path).Retired) != 1 {
		t.Fatal("a pending item was swept inside its grace")
	}
	c.Advance(pendingGrace + time.Second)
	setKeyring(t, s, "another", []byte("another-value-1234"))
	if cat := readCatalog(t, path); len(cat.Retired) != 0 || v.count() != 3 {
		t.Fatalf("vault holds %d items (want 3: one per ref); retired %+v", v.count(), cat.Retired)
	}
}

func TestADeleteThatFailsLeavesARetiredRecordRetriedLater(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	v.failDelete = true
	setKeyring(t, s, "gh/token", []byte("second-value-1234"))

	if cat := readCatalog(t, path); len(cat.Retired) != 1 || cat.Retired[0].Pending || v.count() != 2 {
		t.Fatalf("vault holds %d items; retired %+v", v.count(), cat.Retired)
	}
	got, err := s.Get(context.Background(), "gh/token")
	if err != nil || string(got.Value) != "second-value-1234" {
		t.Fatalf("Get = %q, %v", got.Value, err)
	}

	v.failDelete = false
	if _, err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cat := readCatalog(t, path); len(cat.Retired) != 0 || v.count() != 1 {
		t.Fatalf("vault holds %d items; retired %+v", v.count(), cat.Retired)
	}
}

func TestDeleteForgetsTheEntryEvenIfTheKeyringIsLocked(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", sentinel)
	v.locked = true
	if err := s.Delete(context.Background(), "gh/token"); err != nil {
		t.Fatal(err)
	}
	cat := readCatalog(t, path)
	if len(cat.Secrets) != 0 || len(cat.Retired) != 1 {
		t.Fatalf("catalog %+v", cat)
	}
	v.locked = false
	if _, err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v.count() != 0 || len(readCatalog(t, path).Retired) != 0 {
		t.Fatal("the retired item was not deleted once the keyring unlocked")
	}
}

func TestOrphanEntryIsBrokenAndDeleteRepairsIt(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", sentinel)
	setKeyring(t, s, "ok/token", sentinel)
	cat := readCatalog(t, path)
	delete(v.items, account(cat.CatalogID, "gh/token", cat.Secrets["gh/token"].Item)) // removed from the keyring by hand

	if got := statusOf(t, s, "gh/token"); got != secrets.StateBroken {
		t.Fatalf("orphan is %q", got)
	}
	if got := statusOf(t, s, "ok/token"); got != secrets.StateOK {
		t.Fatalf("intact entry is %q", got)
	}
	if _, err := s.Get(context.Background(), "gh/token"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get of an orphan = %v", err)
	}
	if err := s.Delete(context.Background(), "gh/token"); err != nil {
		t.Fatalf("Delete of an orphan: %v", err)
	}
	if len(readCatalog(t, path).Retired) != 0 {
		t.Fatal("the missing item stayed retired")
	}
	setKeyring(t, s, "gh/token", sentinel)
	if got := statusOf(t, s, "gh/token"); got != secrets.StateOK {
		t.Fatalf("after set again: %q", got)
	}
}

func TestLockedKeyringIsUnavailableNotNotFound(t *testing.T) {
	s, v, _, _ := newKeyringStore(t)
	setKeyring(t, s, "gh/token", sentinel)
	mustSet(t, s, "plain", []byte("plain-value-1234"))
	v.locked = true

	if got := statusOf(t, s, "gh/token"); got != secrets.StateUnavailable {
		t.Fatalf("locked entry is %q", got)
	}
	if got := statusOf(t, s, "plain"); got != secrets.StateOK {
		t.Fatalf("file entry is %q", got)
	}
	_, err := s.Get(context.Background(), "gh/token")
	if !errors.Is(err, secrets.ErrUnavailable) || errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if strings.Contains(err.Error(), string(sentinel)) {
		t.Fatalf("error carries the value: %v", err)
	}
	if metas, err := s.List(context.Background()); err != nil || len(metas) != 2 {
		t.Fatalf("List = %v, %v", metas, err)
	}
}

func TestFailedVaultWriteChangesNothing(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	v.locked = true
	err := s.Set(context.Background(), "gh/token", sentinel, nil, secrets.BackendKeyring)
	if !errors.Is(err, secrets.ErrUnavailable) || strings.Contains(err.Error(), string(sentinel)) {
		t.Fatalf("Set = %v", err)
	}
	v.locked = false
	got, gerr := s.Get(context.Background(), "gh/token")
	if gerr != nil || string(got.Value) != "first-value-1234" {
		t.Fatalf("Get = %q, %v", got.Value, gerr)
	}
	if len(readCatalog(t, path).Retired) != 1 {
		t.Fatal("the unwritten item should be pending for a sweep")
	}
}

func TestKeyringNeedsAVault(t *testing.T) {
	withVault, _, _, path := newKeyringStore(t)
	setKeyring(t, withVault, "gh/token", sentinel)

	without := New(path, clock.NewFake(time.Now()))
	if err := without.Set(context.Background(), "x", sentinel, nil, secrets.BackendKeyring); !errors.Is(err, secrets.ErrUnsupportedBackend) {
		t.Fatalf("Set = %v", err)
	}
	if _, err := without.Get(context.Background(), "gh/token"); !errors.Is(err, secrets.ErrUnsupportedBackend) {
		t.Fatalf("Get = %v", err)
	}
	if got := statusOf(t, without, "gh/token"); got != secrets.StateUnavailable {
		t.Fatalf("Check = %q", got)
	}
	if err := withVault.Set(context.Background(), "x", sentinel, nil, "vault-of-the-moon"); !errors.Is(err, secrets.ErrUnsupportedBackend) {
		t.Fatalf("unknown backend: %v", err)
	}
}

func TestFileSetOverAKeyringEntryRetiresItsItem(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	mustSet(t, s, "gh/token", []byte("second-value-1234"))
	setKeyring(t, s, "other", []byte("other-value-1234")) // sweeps
	if v.count() != 1 || len(readCatalog(t, path).Retired) != 0 {
		t.Fatalf("vault holds %d items", v.count())
	}
}
