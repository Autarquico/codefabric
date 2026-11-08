package blob

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Invariant: once a Put of a blob has returned, the blob exists, whatever a
// Delete that decided on an older mtime does meanwhile. The seam puts the blob
// right after Delete looked at its mtime; the lock is what makes that Put wait
// for the unlink and install the blob anew.
func TestDelete_ARacingPutLeavesTheBlobInstalled(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	s, err := New(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("put while it is being deleted")
	d, _, err := s.Put(t.Context(), bytes.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}
	old := clk.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.path(d), old, old); err != nil {
		t.Fatal(err)
	}
	notAfter := clk.Now().Add(-24 * time.Hour)

	putDone := make(chan error, 1)
	s.afterStat = func() {
		go func() {
			_, _, err := s.Put(context.Background(), bytes.NewReader(content), "")
			putDone <- err
		}()
		select { // without the lock the Put finishes here; with it, it waits for the unlink
		case <-putDone:
			putDone <- nil
		case <-time.After(200 * time.Millisecond):
		}
	}
	if _, err := s.Delete(d, notAfter); err != nil {
		t.Fatal(err)
	}
	if err := <-putDone; err != nil {
		t.Fatalf("Put: %v", err)
	}
	if has, err := s.Has(d); err != nil || !has {
		t.Fatalf("Has = %v, %v: a Put that returned ok left no blob", has, err)
	}
}
