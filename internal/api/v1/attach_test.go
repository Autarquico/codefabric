package v1

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
)

func TestFrames_RoundTrip(t *testing.T) {
	peers, err := PeersFrame([]AttachPeer{{ID: "client_1", Name: "laptop", Mode: AttachModeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	frames := []Frame{
		DataFrame([]byte("ls\r")), DataFrame(nil), ResizeFrame(120, 40), CloseFrame(3, "exited"),
		ErrorFrame(domain.CodeWorkerOffline, "gone"), peers,
	}
	for _, f := range frames {
		if err := WriteFrame(&wire, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range frames {
		got, err := ReadFrame(&wire)
		if err != nil || got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d = %+v (%v), want %+v", i, got, err, want)
		}
	}
	if _, err := ReadFrame(&wire); !errors.Is(err, io.EOF) {
		t.Errorf("after the last frame: %v, want EOF", err)
	}
}

func TestFrames_TypedPayloads(t *testing.T) {
	if c, r, err := ParseResize(ResizeFrame(200, 50).Payload); err != nil || c != 200 || r != 50 {
		t.Errorf("resize = %d %d %v", c, r, err)
	}
	if code, msg, err := ParseClose(CloseFrame(7, "bye").Payload); err != nil || code != 7 || msg != "bye" {
		t.Errorf("close = %d %q %v", code, msg, err)
	}
	e, err := ParseError(ErrorFrame(domain.CodeStaleLease, "moved").Payload)
	if err != nil || e.Code != domain.CodeStaleLease || e.Message != "moved" {
		t.Errorf("error = %+v %v", e, err)
	}
	pf, _ := PeersFrame([]AttachPeer{{ID: "a", Mode: AttachModeView}, {ID: "b", Mode: AttachModeWrite}})
	if got, err := ParsePeers(pf.Payload); err != nil || len(got) != 2 || got[1].ID != "b" {
		t.Errorf("peers = %+v %v", got, err)
	}
	long := strings.Repeat("é", 4000)
	if _, msg, err := ParseClose(CloseFrame(0, long).Payload); err != nil || len(msg) > 1<<10 {
		t.Errorf("a long message is clipped on a rune boundary: %d bytes, %v", len(msg), err)
	}
}

func TestReadFrame_MalformedInput(t *testing.T) {
	header := func(t byte, n uint32) []byte { return binary.BigEndian.AppendUint32([]byte{t}, n) }
	for _, tt := range []struct {
		name string
		in   []byte
		want error
	}{
		{"empty is a clean end", nil, io.EOF},
		{"a header cut short", []byte{1, 0, 0}, io.ErrUnexpectedEOF},
		{"type zero", header(0, 0), ErrBadFrame},
		{"unknown type", header(6, 0), ErrBadFrame},
		{"type 255", header(255, 0), ErrBadFrame},
		{"a length over the maximum", header(1, MaxFramePayload+1), ErrBadFrame},
		{"the biggest length of all", header(1, 1<<32-1), ErrBadFrame},
		{"a payload cut short", append(header(1, 10), 'a', 'b'), io.ErrUnexpectedEOF},
		{"a payload that never starts", header(1, 10), io.ErrUnexpectedEOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ReadFrame(bytes.NewReader(tt.in)); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	// A lying length is refused before anything is allocated for it.
	lying := &countingReader{r: bytes.NewReader(header(1, 1<<31))}
	if _, err := ReadFrame(lying); !errors.Is(err, ErrBadFrame) {
		t.Errorf("err = %v", err)
	}
	if lying.n > len(header(1, 1<<31)) {
		t.Errorf("read %d bytes past the header of a lying length", lying.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestWriteFrame_RefusesWhatCannotBeRead(t *testing.T) {
	if err := WriteFrame(io.Discard, Frame{Type: 0}); !errors.Is(err, ErrBadFrame) {
		t.Errorf("type 0: %v", err)
	}
	if err := WriteFrame(io.Discard, DataFrame(make([]byte, MaxFramePayload+1))); !errors.Is(err, ErrBadFrame) {
		t.Errorf("too big: %v", err)
	}
	if err := WriteFrame(io.Discard, DataFrame(make([]byte, MaxFramePayload))); err != nil {
		t.Errorf("the maximum: %v", err)
	}
}

func TestParse_MalformedPayloads(t *testing.T) {
	for _, p := range [][]byte{nil, {1}, {1, 2, 3}, {1, 2, 3, 4, 5}} {
		if _, _, err := ParseResize(p); !errors.Is(err, ErrBadFrame) {
			t.Errorf("resize %v: %v", p, err)
		}
	}
	for _, p := range [][]byte{nil, {0, 0, 0}, {0, 0, 0, 0, 0xff, 0xfe}} {
		if _, _, err := ParseClose(p); !errors.Is(err, ErrBadFrame) {
			t.Errorf("close %v: %v", p, err)
		}
	}
	for _, p := range []string{"", "x", "{}", `{"code":""}`, `[1]`} {
		if _, err := ParseError([]byte(p)); !errors.Is(err, ErrBadFrame) {
			t.Errorf("error %q: %v", p, err)
		}
	}
	for _, p := range []string{"", "x", `{"id":"a"}`} {
		if _, err := ParsePeers([]byte(p)); !errors.Is(err, ErrBadFrame) {
			t.Errorf("peers %q: %v", p, err)
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 2, 'h', 'i'})
	f.Add([]byte{5, 0, 0, 0, 2, '[', ']'})
	f.Fuzz(func(t *testing.T, in []byte) {
		fr, err := ReadFrame(bytes.NewReader(in))
		if err != nil {
			return
		}
		// What is read is what is written.
		var out bytes.Buffer
		if err := WriteFrame(&out, fr); err != nil || !bytes.HasPrefix(in, out.Bytes()) {
			t.Fatalf("read %+v from %x, wrote %x (%v)", fr, in, out.Bytes(), err)
		}
	})
}

func TestClampScreenHz(t *testing.T) {
	for in, want := range map[int]int{-5: ScreenMinHz, 0: ScreenDefaultHz, 1: 1, 2: 2, 4: 4, 5: ScreenMaxHz, 1000: ScreenMaxHz} {
		if got := ClampScreenHz(in); got != want {
			t.Errorf("ClampScreenHz(%d) = %d, want %d", in, got, want)
		}
	}
}
