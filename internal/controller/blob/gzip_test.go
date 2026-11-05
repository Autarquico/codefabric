package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Autarquico/codefabric/internal/controller/blob"
)

func TestPutGzip_StoresTheCompressedBytesAndOpenGzipReadsTheRawOnes(t *testing.T) {
	s, _ := newStore(t)
	raw := bytes.Repeat([]byte("a log line\n"), 10000)
	d, size, err := s.PutGzip(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 || size >= int64(len(raw)) {
		t.Fatalf("stored %d bytes for %d raw", size, len(raw))
	}
	// The digest is that of what is stored, so the blob verifies as any other.
	stored, err := s.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	packed, _ := io.ReadAll(stored)
	_ = stored.Close()
	if digestOf(string(packed)) != d || int64(len(packed)) != size {
		t.Fatalf("digest %s does not match the %d stored bytes", d, len(packed))
	}
	rc, err := s.OpenGzip(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, raw) || rc.Close() != nil {
		t.Fatalf("read back %d bytes: %v", len(got), err)
	}
	// The same raw bytes are the same blob: writing again is a no-op.
	if d2, _, err := s.PutGzip(context.Background(), raw); err != nil || d2 != d {
		t.Fatalf("second put = %s, %v", d2, err)
	}
}

func TestOpenGzip_ABlobThatIsNotGzipIsCorrupt(t *testing.T) {
	s, _ := newStore(t)
	d, _, err := s.Put(context.Background(), bytes.NewReader([]byte("plain text, not gzip")), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenGzip(d); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.OpenGzip(digestOf("nothing")); err == nil {
		t.Fatal("opened a blob that is not there")
	}
}
