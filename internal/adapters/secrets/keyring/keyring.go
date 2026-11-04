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
	"sync"
	"time"

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

// OpTimeout bounds every operation on the keyring: a locked one may wait for
// an unlock prompt nobody is there to answer.
const OpTimeout = 10 * time.Second

// Backend is the part of go-keyring this package uses.
type Backend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type system struct{}

func (system) Get(s, u string) (string, error) { return zk.Get(s, u) }
func (system) Set(s, u, p string) error        { return zk.Set(s, u, p) }
func (system) Delete(s, u string) error        { return zk.Delete(s, u) }

// Vault is the secrets.Vault of the OS keyring.
type Vault struct {
	b       Backend
	timeout time.Duration // OpTimeout when zero
	writing *writes       // nil: not tracked
}

var _ secrets.Vault = Vault{}

// New is the Vault of the OS keyring.
func New() Vault { return NewWith(system{}, OpTimeout) }

// NewWith is a Vault over b whose operations give up after timeout.
func NewWith(b Backend, timeout time.Duration) Vault {
	return Vault{b: b, timeout: timeout, writing: &writes{}}
}

// writes counts, by account, the Sets whose goroutine has not returned: one
// abandoned by its caller still ends by writing the item.
type writes struct {
	mu sync.Mutex
	n  map[string]int
}

func (w *writes) begin(account string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n == nil {
		w.n = map[string]int{}
	}
	w.n[account]++
}

func (w *writes) end(account string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n[account]--; w.n[account] <= 0 {
		delete(w.n, account)
	}
}

// Writing says whether a Set of account may still complete, though its caller
// has stopped waiting for it.
func (v Vault) Writing(account string) bool {
	if v.writing == nil {
		return false
	}
	v.writing.mu.Lock()
	defer v.writing.mu.Unlock()
	return v.writing.n[account] > 0
}

// Get implements secrets.Vault.
func (v Vault) Get(ctx context.Context, account string) ([]byte, error) {
	s, err := call(ctx, v.timeout, func() (string, error) { return v.b.Get(Service, account) })
	if err != nil {
		return nil, classify(err)
	}
	return []byte(s), nil
}

// Set implements secrets.Vault. A value beyond MaxValueLen is refused before
// the keyring is touched. The value is copied before the call is launched: the
// caller may clear its buffer as soon as Set returns, though the call may not
// have finished.
func (v Vault) Set(ctx context.Context, account string, value []byte) error {
	if len(value) > MaxValueLen {
		return fmt.Errorf("%w: the keyring takes values of at most %d bytes, this has %d",
			domain.ErrInvalidSecret, MaxValueLen, len(value))
	}
	password := string(value)
	v.writing.begin(account)
	_, err := call(ctx, v.timeout, func() (struct{}, error) {
		defer v.writing.end(account)
		return struct{}{}, v.b.Set(Service, account, password)
	})
	return classify(err)
}

// Delete implements secrets.Vault.
func (v Vault) Delete(ctx context.Context, account string) error {
	_, err := call(ctx, v.timeout, func() (struct{}, error) { return struct{}{}, v.b.Delete(Service, account) })
	return classify(err)
}

// errTimeout is an operation that outlived its bound while its caller still
// waited, as opposed to a caller that gave up.
var errTimeout = errors.New("the keyring did not answer in time")

// call runs f, giving up when ctx ends or after timeout: the library has no
// context, and a locked keyring may wait for an unlock prompt. The abandoned
// call finishes in its goroutine.
func call[T any](ctx context.Context, timeout time.Duration, f func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if timeout <= 0 {
		timeout = OpTimeout
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
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-timer.C:
		return zero, errTimeout
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
	case errors.Is(err, errTimeout):
		return fmt.Errorf("%w: the keyring did not answer in time", secrets.ErrUnavailable)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", secrets.ErrUnavailable, err)
	default:
		return fmt.Errorf("%w: the keyring is locked, denied access or not running", secrets.ErrUnavailable)
	}
}
