package keyring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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
}

func (f *fakeBackend) Get(string, string) (string, error) {
	if f.block != nil {
		<-f.block
	}
	return "", f.getErr
}

func (f *fakeBackend) Set(string, string, string) error { f.setCalls++; return f.setErr }
func (f *fakeBackend) Delete(string, string) error      { return f.setErr }

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
			v := Vault{b: &fakeBackend{getErr: tc.err}}
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
	v := Vault{b: b}
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
	if _, err := (Vault{b: b}).Get(ctx, "a"); !errors.Is(err, secrets.ErrUnavailable) {
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
