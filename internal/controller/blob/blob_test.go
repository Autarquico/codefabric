package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func digestOf(content string) blob.Digest {
	sum := sha256.Sum256([]byte(content))
	return blob.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func newStore(t *testing.T) (*blob.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := blob.New(root, clock.Real{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, root
}

func tempFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatalf("reading tmp: %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func TestScenario_ControllerStore_BlobFalseDigest(t *testing.T) {
	s, root := newStore(t)
	wrong := digestOf("something else")

	_, _, err := s.Put(t.Context(), strings.NewReader("payload"), wrong)

	if !errors.Is(err, blob.ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Fatalf("temporaries left behind: %v", left)
	}
	if ok, _ := s.Has(wrong); ok {
		t.Fatal("blob stored under the false digest")
	}
	if ok, _ := s.Has(digestOf("payload")); ok {
		t.Fatal("rejected content was stored under its real digest")
	}
}

func TestPut_StoresAndReadsBack(t *testing.T) {
	s, root := newStore(t)
	want := digestOf("hello")

	got, size, err := s.Put(t.Context(), strings.NewReader("hello"), want)
	if err != nil || got != want || size != 5 {
		t.Fatalf("Put = (%s, %d, %v), want (%s, 5, nil)", got, size, err, want)
	}

	rc, err := s.Open(got)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if data, _ := io.ReadAll(rc); string(data) != "hello" {
		t.Fatalf("content = %q", data)
	}
	h := strings.TrimPrefix(string(want), "sha256:")
	if _, err := os.Stat(filepath.Join(root, "blobs", "sha256", h[:2], h[2:4], h)); err != nil {
		t.Fatalf("blob not at the content-addressed path: %v", err)
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Fatalf("temporaries left behind: %v", left)
	}
}

func TestPut_WithoutExpectedDigestComputesIt(t *testing.T) {
	s, _ := newStore(t)
	got, _, err := s.Put(t.Context(), strings.NewReader("abc"), "")
	if err != nil || got != digestOf("abc") {
		t.Fatalf("Put = (%s, %v)", got, err)
	}
}

func TestPut_ExistingBlobIsNotRewritten(t *testing.T) {
	s, root := newStore(t)
	d, _, err := s.Put(t.Context(), strings.NewReader("same"), "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "blobs", "sha256", string(d)[7:9], string(d)[9:11], string(d)[7:])
	before, _ := os.Stat(path)

	if _, _, err := s.Put(t.Context(), strings.NewReader("same"), d); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("existing blob was replaced")
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Fatalf("temporaries left behind: %v", left)
	}
}

func TestPut_CorruptExistingBlobIsReportedAndKept(t *testing.T) {
	s, root := newStore(t)
	d := digestOf("pristine")
	h := string(d)[7:]
	dir := filepath.Join(root, "blobs", "sha256", h[:2], h[2:4])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, h)
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.Put(t.Context(), strings.NewReader("pristine"), d)

	if !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("err = %v, want ErrCorruptBlob", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "tampered" { //nolint:gosec // test path
		t.Fatal("corrupt blob was modified")
	}
}

func TestPut_ConcurrentSameDigest(t *testing.T) {
	s, root := newStore(t)
	content := strings.Repeat("x", 1<<20)
	want := digestOf(content)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, _, err := s.Put(t.Context(), strings.NewReader(content), want)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Put: %v", err)
		}
	}

	rc, err := s.Open(want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(rc)
	if !bytes.Equal(data, []byte(content)) {
		t.Fatal("stored content differs")
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Fatalf("temporaries left behind: %v", left)
	}
}

func TestPut_CanceledContextLeavesNothing(t *testing.T) {
	s, root := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, _, err := s.Put(ctx, strings.NewReader("data"), ""); err == nil {
		t.Fatal("expected an error")
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Fatalf("temporaries left behind: %v", left)
	}
}

func TestOpen_MissingBlobIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Open(digestOf("absent"))
	if !errors.Is(err, domain.NewError(domain.CodeNotFound, "", nil)) {
		t.Fatalf("err = %v, want CF_NOT_FOUND", err)
	}
}

func TestParseDigest(t *testing.T) {
	valid := string(digestOf("x"))
	cases := map[string]bool{
		valid:                                  true,
		"sha256:abc":                           false,
		"md5:" + valid[7:]:                     false,
		strings.ToUpper(valid[:7]) + valid[7:]: false,
		valid[:7] + strings.ToUpper(valid[7:]): false,
		valid[:len(valid)-1] + "g":             false,
		"":                                     false,
	}
	for in, ok := range cases {
		_, err := blob.ParseDigest(in)
		if (err == nil) != ok {
			t.Errorf("ParseDigest(%q) err = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestNew_RemovesOnlyStaleTemporaries(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale, fresh := filepath.Join(tmp, "put-stale"), filepath.Join(tmp, "put-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := blob.New(root, clock.NewFake(now)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temporary survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh temporary removed: %v", err)
	}
}

func TestHasAndOpen_RejectMalformedDigest(t *testing.T) {
	s, _ := newStore(t)
	for _, raw := range []string{"", "x", "sha256:../../etc", "sha256:" + strings.Repeat("A", 64)} {
		d := blob.Digest(raw)
		if _, err := s.Has(d); !errors.Is(err, blob.ErrMalformedDigest) {
			t.Errorf("Has(%q) error = %v, want ErrMalformedDigest", raw, err)
		}
		if _, err := s.Open(d); !errors.Is(err, blob.ErrMalformedDigest) {
			t.Errorf("Open(%q) error = %v, want ErrMalformedDigest", raw, err)
		}
	}
}

func TestPut_RejectsMalformedWantWithoutTempFiles(t *testing.T) {
	s, root := newStore(t)
	_, _, err := s.Put(t.Context(), strings.NewReader("payload"), "sha256:../x")
	if !errors.Is(err, blob.ErrMalformedDigest) {
		t.Fatalf("Put error = %v, want ErrMalformedDigest", err)
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Errorf("temporaries left behind: %v", left)
	}
}
