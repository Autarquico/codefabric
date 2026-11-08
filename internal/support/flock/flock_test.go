package flock_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/flock"
)

func TestLock_ExcludesASecondHolderUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	release, err := flock.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		r, err := flock.Lock(path)
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("a second holder got the lock while the first still had it")
	case <-time.After(100 * time.Millisecond): // real time: waiting for something that must not happen
	}
	release()
	select {
	case r := <-got:
		r()
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never got the lock after the release")
	}
}
