package file

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/adapters/secrets/secretstest"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// fakeVault is a keyring in memory.
type fakeVault struct {
	mu    sync.Mutex
	items map[string][]byte
	// locked makes every call ErrUnavailable; failDelete only Delete.
	locked, failDelete bool
	// setErr, if set, is what Set refuses with before writing.
	setErr error
	gets   int
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
	if v.setErr != nil {
		v.mu.Unlock()
		return v.setErr
	}
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
	secretstest.RequireValueOutOfCatalog(t, raw, "gh/token", sentinel)
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
	// The sweep removed an item that existed: the write landed and is cleaned,
	// which is the evidence that lets the record go.
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
	if len(readCatalog(t, path).Retired) != 0 {
		t.Fatal("a write that finished without writing leaves nothing to sweep")
	}
}

// Failed writes that definitely wrote nothing must not exhaust the backlog.
func TestFailedKeyringSetsDoNotFillTheBacklog(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	ctx := context.Background()
	v.locked = true
	for i := range maxRetired + 5 {
		err := s.Set(ctx, fmt.Sprintf("ref%03d", i), sentinel, nil, secrets.BackendKeyring)
		if !errors.Is(err, secrets.ErrUnavailable) || strings.Contains(err.Error(), "waiting to be cleaned up") {
			t.Fatalf("Set %d = %v", i, err)
		}
	}
	if n := len(readCatalog(t, path).Retired); n != 0 {
		t.Fatalf("%d records after failed writes", n)
	}
	v.locked = false
	setKeyring(t, s, "gh/token", []byte("good-value-1234"))
	if n := len(readCatalog(t, path).Retired); n != 0 || v.count() != 1 {
		t.Fatalf("%d records, %d items", n, v.count())
	}
}

func TestASetTheKeyringRefusedOutrightLeavesNoPendingRecord(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	v.setErr = domain.ErrInvalidSecret
	if err := s.Set(context.Background(), "gh/token", sentinel, nil, secrets.BackendKeyring); !errors.Is(err, domain.ErrInvalidSecret) {
		t.Fatalf("Set = %v", err)
	}
	if n := len(readCatalog(t, path).Retired); n != 0 {
		t.Fatalf("%d retired records after a Set nothing was written for", n)
	}
}

// A Set whose keyring write outlives pendingGrace must lose to the sweep that
// deletes its item: never an entry pointing to a deleted item.
func TestASetThatOutlivesTheGraceNeverCommitsAPointerToASweptItem(t *testing.T) {
	for name, sweepFirst := range map[string]bool{"swept before the swap": true, "grace passed, not swept yet": false} {
		t.Run(name, func(t *testing.T) {
			s, v, c, _ := newKeyringStore(t)
			setKeyring(t, s, "gh/token", []byte("first-value-1234"))
			v.afterSet = func(string) {
				c.Advance(pendingGrace)
				if sweepFirst {
					s.sweep(context.Background())
				}
			}
			err := s.Set(context.Background(), "gh/token", []byte("second-value-1234"), nil, secrets.BackendKeyring)
			v.afterSet = nil
			if err == nil {
				t.Fatal("the late Set should fail")
			}
			got, gerr := s.Get(context.Background(), "gh/token")
			if gerr != nil || string(got.Value) != "first-value-1234" {
				t.Fatalf("Get = %q, %v: the old version should be intact", got.Value, gerr)
			}
			if v.count() != 1 {
				t.Fatalf("the vault holds %d items, want only the old one", v.count())
			}
		})
	}
}

// copyCatalog copies the directory of the catalog at path, as a restore or a
// cloned setup would.
func copyCatalog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "worker")
	if err := os.Mkdir(dir, 0o700); err != nil { //nolint:gosec // the catalog directory is 0700, as in production
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(copied, data, 0o600); err != nil { //nolint:gosec // a path of the test
		t.Fatal(err)
	}
	return copied
}

func TestACopiedCatalogReadsButNeverWritesOrDeletesTheKeyringItemsOfTheOriginal(t *testing.T) {
	ctx := context.Background()
	s, v, c, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	setKeyring(t, s, "gh/other", []byte("other-value-1234"))
	clone := New(copyCatalog(t, path), c, WithVault(v))

	if got, err := clone.Get(ctx, "gh/token"); err != nil || string(got.Value) != "first-value-1234" {
		t.Fatalf("Get on the copy = %q, %v", got.Value, err)
	}
	if err := clone.Set(ctx, "gh/token", []byte("changed-value-1234"), nil, secrets.BackendKeyring); !errors.Is(err, secrets.ErrCopied) {
		t.Fatalf("keyring Set on the copy = %v, want ErrCopied", err)
	}
	if err := clone.Set(ctx, "gh/token", []byte("changed-value-1234"), nil, secrets.BackendFile); err != nil {
		t.Fatalf("file Set over a keyring entry on the copy: %v", err)
	}
	if err := clone.Delete(ctx, "gh/other"); err != nil {
		t.Fatalf("Delete on the copy: %v", err)
	}
	c.Advance(pendingGrace)
	if _, err := clone.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if v.count() != 2 {
		t.Fatalf("the vault holds %d items, want the 2 of the original", v.count())
	}
	for _, ref := range []string{"gh/token", "gh/other"} {
		if got, err := s.Get(ctx, ref); err != nil || len(got.Value) == 0 {
			t.Fatalf("the original lost %q: %v", ref, err)
		}
	}

	// With no keyring entry left, nothing is shared: the copy keeps a namespace of its own.
	if err := clone.Set(ctx, "own/token", []byte("own-value-123456"), nil, secrets.BackendKeyring); err != nil {
		t.Fatalf("keyring Set on a copy that shares nothing: %v", err)
	}
	if v.count() != 3 || readCatalog(t, clone.path).CatalogID == readCatalog(t, path).CatalogID {
		t.Fatalf("the copy should have a catalog id of its own; vault holds %d", v.count())
	}
}

func TestAMovedCatalogIsNotACopy(t *testing.T) {
	s, v, c, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(filepath.Dir(path), moved); err != nil {
		t.Fatal(err)
	}
	s = New(filepath.Join(moved, "secrets.json"), c, WithVault(v))
	setKeyring(t, s, "gh/token", []byte("second-value-1234"))
	if v.count() != 1 {
		t.Fatalf("the vault holds %d items, want 1: the old one swept", v.count())
	}
}

// rewriteCatalog changes the catalog at path as edit says, keeping its mode.
func rewriteCatalog(t *testing.T, path string, edit func(*catalog)) {
	t.Helper()
	c := readCatalog(t, path)
	edit(&c)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // a path of the test
		t.Fatal(err)
	}
}

func TestACatalogWhoseRecordedDeviceDiffersIsNotACopy(t *testing.T) {
	ctx := context.Background()
	s, v, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	rewriteCatalog(t, path, func(c *catalog) { c.Origin = "424242:" + inodeOf(c.Origin) })

	if c, err := s.load(); err != nil || c.copied {
		t.Fatalf("load: copied = %v, err = %v; a renumbered device must not make a copy", c.copied, err)
	}
	setKeyring(t, s, "gh/token", []byte("second-value-1234"))
	if err := s.Delete(ctx, "gh/token"); err != nil {
		t.Fatal(err)
	}
	setKeyring(t, s, "other", []byte("other-value-1234")) // sweeps
	if v.count() != 1 {
		t.Fatalf("the vault holds %d items, want 1: the retired ones swept", v.count())
	}
}

func TestACatalogWithKeyringEntriesAndNoOriginIsStampedNotFlagged(t *testing.T) {
	ctx := context.Background()
	s, _, _, path := newKeyringStore(t)
	setKeyring(t, s, "gh/token", []byte("first-value-1234"))
	rewriteCatalog(t, path, func(c *catalog) { c.Origin = "" })

	if c, err := s.load(); err != nil || c.copied {
		t.Fatalf("load: copied = %v, err = %v", c.copied, err)
	}
	if err := s.Set(ctx, "gh/token", []byte("second-value-1234"), nil, secrets.BackendKeyring); err != nil {
		t.Fatalf("keyring Set on a catalog with no origin: %v", err)
	}
	if got := readCatalog(t, path); got.Origin == "" || got.Origin != dirIdentity(filepath.Dir(path)) {
		t.Fatalf("origin = %q, want the directory's identity", got.Origin)
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

// abandoningVault is a Vault whose Set gives up at once, as one cut by its
// context does, while the write goes on: land makes it reach the shared backend.
type abandoningVault struct {
	*fakeVault
	account string
	value   []byte
}

func (a *abandoningVault) Set(_ context.Context, account string, value []byte) error {
	a.account, a.value = account, bytes.Clone(value)
	return fmt.Errorf("%w: %w", secrets.ErrAbandoned, secrets.ErrUnavailable)
}

func (a *abandoningVault) land() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items[a.account] = a.value
}

// A Set abandoned by its caller may still land. Another Store, over the same
// backend and catalog, sweeps before it does and finds nothing to delete: it
// must keep the record, so that a later sweep deletes the late item.
func TestASweepOfAnotherStoreStillCleansAnAbandonedSetThatLandsLate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker", "secrets.json")
	c := clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	backend := newFakeVault()
	writer := New(path, c, WithVault(&abandoningVault{fakeVault: backend}))
	v := writer.vault.(*abandoningVault)
	other := New(path, c, WithVault(backend))
	ctx := context.Background()

	if err := writer.Set(ctx, "gh/token", []byte("first-value-1234"), nil, secrets.BackendKeyring); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("Set = %v", err)
	}
	c.Advance(pendingGrace + time.Minute)
	for range 20 { // many sweeps, a long time: the record stays while nothing was deleted
		other.sweep(ctx)
		c.Advance(24 * time.Hour)
	}
	if cat := readCatalog(t, path); len(cat.Retired) != 1 || !cat.Retired[0].Pending {
		t.Fatalf("retired = %+v: forgotten while the write may land", cat.Retired)
	}

	v.land()
	if backend.count() != 1 {
		t.Fatalf("the vault holds %d items after the late write", backend.count())
	}
	other.sweep(ctx)
	if cat := readCatalog(t, path); backend.count() != 0 || len(cat.Retired) != 0 {
		t.Fatalf("the late item was left: %d items, retired %+v", backend.count(), cat.Retired)
	}
}

// lateVault abandons every Set and lets the test say how each ended.
type lateVault struct {
	*fakeVault
	late []func(error)
}

func (l *lateVault) Set(ctx context.Context, _ string, _ []byte) error {
	l.late = append(l.late, secrets.LateOutcome(ctx))
	return fmt.Errorf("%w: %w", secrets.ErrAbandoned, secrets.ErrUnavailable)
}

// Abandoned writes that later fail wrote nothing: their records go, and the
// backlog they filled empties.
func TestAbandonedWritesThatFailLateFreeTheBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker", "secrets.json")
	c := clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	lv := &lateVault{fakeVault: newFakeVault()}
	s := New(path, c, WithVault(lv))
	ctx := context.Background()
	for i := range maxRetired {
		if err := s.Set(ctx, fmt.Sprintf("ref%03d", i), sentinel, nil, secrets.BackendKeyring); !errors.Is(err, secrets.ErrAbandoned) {
			t.Fatalf("Set %d = %v", i, err)
		}
	}
	if err := s.Set(ctx, "one-more", sentinel, nil, secrets.BackendKeyring); !strings.Contains(fmt.Sprint(err), "waiting to be cleaned up") {
		t.Fatalf("a full backlog accepted a write: %v", err)
	}
	if n := len(readCatalog(t, path).Retired); n != maxRetired {
		t.Fatalf("%d records before the late failures", n)
	}
	for _, fn := range lv.late {
		fn(secrets.ErrUnavailable)
	}
	if n := len(readCatalog(t, path).Retired); n != 0 {
		t.Fatalf("%d records after the late failures", n)
	}
	s = New(path, c, WithVault(newFakeVault()))
	setKeyring(t, s, "gh/token", []byte("good-value-1234"))
}

// At maxRetired records new keyring work is refused and no record is dropped.
func TestBacklogAtTheCapRefusesNewKeyringWorkAndDropsNothing(t *testing.T) {
	s, v, _, path := newKeyringStore(t)
	ctx := context.Background()
	total := maxRetired + 5
	for i := range total {
		setKeyring(t, s, fmt.Sprintf("ref%03d", i), []byte(fmt.Sprintf("value-%03d-1234567890", i)))
	}
	v.locked = true
	var refused int
	for i := range total {
		err := s.Delete(ctx, fmt.Sprintf("ref%03d", i))
		switch {
		case err == nil:
		case errors.Is(err, secrets.ErrUnavailable) && strings.Contains(err.Error(), "waiting to be cleaned up"):
			refused++
		default:
			t.Fatalf("Delete = %v", err)
		}
	}
	cat := readCatalog(t, path)
	if refused != total-maxRetired || len(cat.Retired) != maxRetired || len(cat.Secrets) != total-maxRetired {
		t.Fatalf("refused %d, retired %d, kept %d", refused, len(cat.Retired), len(cat.Secrets))
	}
	if err := s.Set(ctx, "plain", []byte("plain-value-1234"), nil, secrets.BackendFile); err != nil {
		t.Fatalf("a file Set is refused: %v", err)
	}
	if err := s.Set(ctx, "new/token", sentinel, nil, secrets.BackendKeyring); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("a keyring Set at the cap = %v", err)
	}
	v.locked = false
	if _, err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(readCatalog(t, path).Retired) != 0 || v.count() != total-maxRetired {
		t.Fatalf("after unlocking: %d records, %d items", len(readCatalog(t, path).Retired), v.count())
	}
	for _, val := range v.items {
		if !strings.HasPrefix(string(val), "value-") {
			t.Fatalf("unexpected item %q", val)
		}
	}
}
