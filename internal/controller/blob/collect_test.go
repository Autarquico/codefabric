package blob_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func put(t *testing.T, s *blob.Store, content string) blob.Digest {
	t.Helper()
	d, _, err := s.Put(t.Context(), bytes.NewReader([]byte(content)), "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return d
}

func age(t *testing.T, root string, d blob.Digest, at time.Time) {
	t.Helper()
	h := string(d)[len("sha256:"):]
	if err := os.Chtimes(filepath.Join(root, "blobs", "sha256", h[0:2], h[2:4], h), at, at); err != nil {
		t.Fatal(err)
	}
}

func TestWalk_ListsEveryInstalledBlobWithSizeAndMtime(t *testing.T) {
	s, root := newStore(t)
	a, b := put(t, s, "alpha"), put(t, s, "bravo!")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	age(t, root, a, old)

	got := map[blob.Digest]blob.Entry{}
	if err := s.Walk(t.Context(), func(e blob.Entry) error { got[e.Digest] = e; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a].Size != 5 || got[b].Size != 6 || !got[a].ModTime.Equal(old) {
		t.Fatalf("Walk = %+v", got)
	}
}

func TestWalk_OnAnEmptyStoreIsNothing(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Walk(t.Context(), func(blob.Entry) error { t.Fatal("no blob expected"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestDelete_RemovesOnlyABlobNotTouchedAfterTheCutoff(t *testing.T) {
	s, root := newStore(t)
	old, fresh := put(t, s, "old"), put(t, s, "fresh")
	cutoff := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	age(t, root, old, cutoff.Add(-time.Hour))
	age(t, root, fresh, cutoff.Add(time.Hour))

	if gone, err := s.Delete(old, cutoff); err != nil || !gone {
		t.Fatalf("Delete(old) = %v, %v", gone, err)
	}
	if gone, err := s.Delete(fresh, cutoff); err != nil || gone {
		t.Fatalf("Delete(fresh) = %v, %v; a recent blob must stay", gone, err)
	}
	if has, _ := s.Has(old); has {
		t.Fatal("the old blob is still there")
	}
	if has, _ := s.Has(fresh); !has {
		t.Fatal("the fresh blob was removed")
	}
	if gone, err := s.Delete(old, cutoff); err != nil || gone {
		t.Fatalf("Delete of a blob that is not there = %v, %v", gone, err)
	}
}

func TestPut_OfAnInstalledBlobRefreshesItsMtime(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	s, err := blob.New(root, clk)
	if err != nil {
		t.Fatal(err)
	}
	d := put(t, s, "kept alive")
	age(t, root, d, clk.Now().Add(-48*time.Hour))

	put(t, s, "kept alive")

	var mtime time.Time
	_ = s.Walk(t.Context(), func(e blob.Entry) error { mtime = e.ModTime; return nil })
	if !mtime.Equal(clk.Now()) {
		t.Fatalf("mtime = %v, want the clock's %v", mtime, clk.Now())
	}
}

// A Put of an existing blob that lands between the moment a collector decided
// the blob was old and its unlink must save it: Delete reads the mtime itself.
func TestDelete_AfterAPutOfTheSameBlobKeepsIt(t *testing.T) {
	s, root := newStore(t)
	d := put(t, s, "raced")
	decided := time.Now().Add(-time.Hour) // the collector's cutoff; the blob looked older than it
	age(t, root, d, decided.Add(-time.Hour))

	put(t, s, "raced") // the writer arrives after the decision, before the unlink

	if gone, err := s.Delete(d, decided); err != nil || gone {
		t.Fatalf("Delete = %v, %v; the blob was put again after the cutoff", gone, err)
	}
	if has, _ := s.Has(d); !has {
		t.Fatal("the blob a writer had just put is gone")
	}
}
