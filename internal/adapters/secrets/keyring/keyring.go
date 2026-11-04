// Package keyring is the secrets.Vault over the OS keyring: the macOS Keychain
// (through /usr/bin/security) and the Secret Service on Linux (over D-Bus), by
// github.com/zalando/go-keyring v0.2.8.
//
// On macOS that library hands the value to `security -i` on stdin, base64
// encoded, so it never appears in an argv; it refuses a command longer than
// 4096 bytes only after it has started the process, so this package refuses an
// oversized value first. Errors are classified, never quoted: the library's own
// may echo bytes of what it was decoding.
package keyring

import (
	"context"
	"errors"
	"fmt"

	zk "github.com/zalando/go-keyring"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	// Service is the keyring service every item is stored under.
	Service = "codefabric"

	// MaxValueLen is the largest value a Vault takes. macOS limits the whole
	// `security -i` command to 4096 bytes: the base64 of 2048 bytes is 2732,
	// and the account (at most 26+1+128+1+26 bytes), the service and the
	// framing leave well over a kilobyte to spare.
	MaxValueLen = 2048
)

// backend is the part of go-keyring this package uses.
type backend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type system struct{}

func (system) Get(s, u string) (string, error) { return zk.Get(s, u) }
func (system) Set(s, u, p string) error        { return zk.Set(s, u, p) }
func (system) Delete(s, u string) error        { return zk.Delete(s, u) }

// Vault is the secrets.Vault of the OS keyring.
type Vault struct{ b backend }

var _ secrets.Vault = Vault{}

// New is the Vault of the OS keyring.
func New() Vault { return Vault{b: system{}} }

// Get implements secrets.Vault.
func (v Vault) Get(ctx context.Context, account string) ([]byte, error) {
	s, err := call(ctx, func() (string, error) { return v.b.Get(Service, account) })
	if err != nil {
		return nil, classify(err)
	}
	return []byte(s), nil
}

// Set implements secrets.Vault. A value beyond MaxValueLen is refused before
// the keyring is touched.
func (v Vault) Set(ctx context.Context, account string, value []byte) error {
	if len(value) > MaxValueLen {
		return fmt.Errorf("%w: the keyring takes values of at most %d bytes, this has %d",
			domain.ErrInvalidSecret, MaxValueLen, len(value))
	}
	_, err := call(ctx, func() (struct{}, error) { return struct{}{}, v.b.Set(Service, account, string(value)) })
	return classify(err)
}

// Delete implements secrets.Vault.
func (v Vault) Delete(ctx context.Context, account string) error {
	_, err := call(ctx, func() (struct{}, error) { return struct{}{}, v.b.Delete(Service, account) })
	return classify(err)
}

// call runs f, giving up when ctx ends: the library has no context, and a
// locked keyring may wait for an unlock prompt. The abandoned call finishes in
// its goroutine.
func call[T any](ctx context.Context, f func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// classify maps an error of the library to the ones of the port, dropping its
// text.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, zk.ErrNotFound):
		return secrets.ErrNotFound
	case errors.Is(err, zk.ErrSetDataTooBig):
		return fmt.Errorf("%w: the keyring refused a value that large", domain.ErrInvalidSecret)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", secrets.ErrUnavailable, err)
	default:
		return fmt.Errorf("%w: the keyring is locked, denied access or not running", secrets.ErrUnavailable)
	}
}
