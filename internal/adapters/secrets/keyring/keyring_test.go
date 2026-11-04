package keyring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	zk "github.com/zalando/go-keyring"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
)

type fakeBackend struct {
	getErr, setErr error
	setCalls       int
	block          chan struct{}
	setBlock       chan struct{}
	setStarted     chan struct{}
	set            chan string
	calls          atomic.Int32
}

func (f *fakeBackend) Get(string, string) (string, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return "", f.getErr
}

func (f *fakeBackend) Set(_, _, p string) error {
	f.setCalls++
	f.calls.Add(1)
	if f.setStarted != nil {
		close(f.setStarted)
	}
	if f.setBlock != nil {
		<-f.setBlock
	}
	if f.set != nil {
		f.set <- p
	}
	return f.setErr
}

func (f *fakeBackend) Delete(string, string) error {
	f.calls.Add(1)
	return f.setErr
}

func TestErrorMapping(t *testing.T) {
	leak := "s3ntinel-VALUE"
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"not found", zk.ErrNotFound, secrets.ErrNotFound},
		{"too big", zk.ErrSetDataTooBig, domain.ErrInvalidSecret},
		{"locked", fmt.Errorf("exit status 36 %s", leak), secrets.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewWith(&fakeBackend{getErr: tc.err}, 0)
			_, err := v.Get(t.Context(), "a")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error quotes the library's: %v", err)
			}
		})
	}
}

func TestSetRefusesOversizedBeforeTouchingTheKeyring(t *testing.T) {
	b := &fakeBackend{}
	v := NewWith(b, 0)
	err := v.Set(t.Context(), "a", bytes.Repeat([]byte("x"), MaxValueLen+1))
	if !errors.Is(err, domain.ErrInvalidSecret) || b.setCalls != 0 {
		t.Fatalf("err=%v setCalls=%d", err, b.setCalls)
	}
	if err := v.Set(t.Context(), "a", bytes.Repeat([]byte("x"), MaxValueLen)); err != nil || b.setCalls != 1 {
		t.Fatalf("err=%v setCalls=%d", err, b.setCalls)
	}
}

func TestBlockedKeyringIsUnavailableOnTimeout(t *testing.T) {
	b := &fakeBackend{block: make(chan struct{})}
	defer close(b.block)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := NewWith(b, 0).Get(ctx, "a"); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// The longest account the file Store builds (namespace/ref/ulid) with the
// largest value must fit the 4096 bytes macOS allows a `security -i` command,
// even if quoting doubled the service and the account.
func TestLargestItemFitsTheMacOSCommand(t *testing.T) {
	account := strings.Repeat("n", 26) + "/" + strings.Repeat("r", domain.MaxSecretRefLen) + "/" + strings.Repeat("u", 26)
	b64 := (MaxValueLen + 2) / 3 * 4
	frame := len("add-generic-password -U -s '' -a '' -w ''\n")
	cmd := frame + 2*(len(Service)+len(account)) + len("go-keyring-base64:") + b64
	if cmd > 4096 {
		t.Fatalf("command of %d bytes", cmd)
	}
}

func TestAnOperationThatNeverAnswersIsUnavailableAfterItsBound(t *testing.T) {
	b := &fakeBackend{block: make(chan struct{})}
	defer close(b.block)
	start := time.Now()
	_, err := NewWith(b, 20*time.Millisecond).Get(t.Context(), "a") // ctx without deadline
	if !errors.Is(err, secrets.ErrUnavailable) || errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

// The goroutine of a Set cancelled midway reads a copy: the caller clears its
// buffer as Set returns (run with -race).
func TestCancelledSetKeepsItsOwnCopyOfTheValue(t *testing.T) {
	b := &fakeBackend{setBlock: make(chan struct{}), setStarted: make(chan struct{}), set: make(chan string, 1)}
	v := NewWith(b, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	value := []byte("s3ntinel-VALUE")
	errc := make(chan error, 1)
	go func() { errc <- v.Set(ctx, "acc", value) }()
	<-b.setStarted
	cancel()
	if err := <-errc; !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	clear(value)
	close(b.setBlock)
	if got := <-b.set; got != "s3ntinel-VALUE" {
		t.Fatalf("the keyring got %q", got)
	}
}

func TestACancelledContextReachesNoBackendAndLeavesNothingBehind(t *testing.T) {
	b := &fakeBackend{}
	v := NewWith(b, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := v.Set(ctx, "acc", []byte("value")); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("Set = %v", err)
	}
	if _, err := v.Get(ctx, "acc"); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("Get = %v", err)
	}
	if n := b.calls.Load(); n != 0 {
		t.Fatalf("%d backend calls", n)
	}
	if len(v.slot) != 0 {
		t.Fatal("the slot is held")
	}
}

// A keyring that never answers holds one operation, not one per attempt; the
// next ones are refused at once, and the slot frees when the operation ends.
func TestAStuckOperationLetsNoSecondOneStart(t *testing.T) {
	b := &fakeBackend{block: make(chan struct{})}
	v := NewWith(b, 20*time.Millisecond)
	if _, err := v.Get(t.Context(), "a"); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("first Get = %v", err)
	}
	start := time.Now()
	for range 5 {
		if err := v.Delete(t.Context(), "a"); !errors.Is(err, secrets.ErrUnavailable) {
			t.Fatalf("Delete while stuck = %v", err)
		}
	}
	if d := time.Since(start); d > 10*time.Millisecond {
		t.Fatalf("refusals took %s: they should not wait", d)
	}
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("%d backend calls, want the one stuck", n)
	}

	close(b.block)
	deadline := time.Now().Add(5 * time.Second)
	for len(v.slot) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slot was never freed")
		}
		time.Sleep(time.Millisecond)
	}
	if err := v.Delete(t.Context(), "a"); err != nil {
		t.Fatalf("Delete after the slot freed = %v", err)
	}
}
