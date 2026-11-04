// Package secretstest is a keyring in memory for the tests of whatever sits on
// a secrets.Vault.
package secretstest

import (
	"bytes"
	"context"
	"strings"
	"sync"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
)

// Vault is a secrets.Vault in memory that can be locked, and whose items a
// test can take away, as a person does in the keychain app.
type Vault struct {
	mu     sync.Mutex
	items  map[string][]byte
	locked bool
}

var _ secrets.Vault = (*Vault)(nil)

// New is an unlocked, empty Vault.
func New() *Vault { return &Vault{items: map[string][]byte{}} }

// Lock makes every call ErrUnavailable until Unlock.
func (v *Vault) Lock() { v.setLocked(true) }

// Unlock undoes Lock.
func (v *Vault) Unlock() { v.setLocked(false) }

func (v *Vault) setLocked(b bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.locked = b
}

// Get implements secrets.Vault.
func (v *Vault) Get(_ context.Context, account string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.locked {
		return nil, secrets.ErrUnavailable
	}
	val, ok := v.items[account]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return bytes.Clone(val), nil
}

// Set implements secrets.Vault.
func (v *Vault) Set(_ context.Context, account string, value []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.locked {
		return secrets.ErrUnavailable
	}
	v.items[account] = bytes.Clone(value)
	return nil
}

// Delete implements secrets.Vault.
func (v *Vault) Delete(_ context.Context, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.locked {
		return secrets.ErrUnavailable
	}
	if _, ok := v.items[account]; !ok {
		return secrets.ErrNotFound
	}
	delete(v.items, account)
	return nil
}

// Lose drops every item of ref, locked or not: the entry is orphaned.
func (v *Vault) Lose(ref string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for account := range v.items {
		if strings.Contains(account, "/"+ref+"/") {
			delete(v.items, account)
		}
	}
}

// Len is the number of items held.
func (v *Vault) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.items)
}
