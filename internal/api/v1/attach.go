package v1

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/domain"
)

// The relay of attach (live-attach spec, D1): a client opens a full-duplex
// stream with the Controller (POST /v1/runs/{ref}/attach/stream), a Worker
// that announced domain.AttachRelayCapability finds the request by long-poll
// (GET /v1/worker/attach-requests) and dials the other end back
// (POST /v1/worker/attach/{sid}); both are HTTP/1.1 Upgrades to AttachUpgrade,
// and from the 101 on each side speaks frames.

// AttachUpgrade is the protocol token of the Upgrade header of both ends.
const AttachUpgrade = "cf-attach/1"

// Modes of an attach. View sends nothing to the Run; write sends what the
// person types; shell is reserved for a shell in the worktree and is refused
// until it exists.
const (
	AttachModeView  = "view"
	AttachModeWrite = "write"
	AttachModeShell = "shell"
	// AttachModeScreen is a read-only stream of the screen of the Run: no
	// terminal is opened on the Worker, which sends the whole screen (one data
	// frame each) whenever it changed, at the rate the client asked for.
	AttachModeScreen = "screen"
)

// The rate of a screen stream, in screens per second: what a client may ask for
// in AttachOpenRequest.Hz and what it gets when it asks for none.
const (
	ScreenMinHz     = 1
	ScreenMaxHz     = 4
	ScreenDefaultHz = 2
)

// ClampScreenHz is the rate a screen stream is granted for the one asked: 0 is
// the default and anything else is kept between the least and the most.
func ClampScreenHz(hz int) int {
	if hz == 0 {
		return ScreenDefaultHz
	}
	return min(max(hz, ScreenMinHz), ScreenMaxHz)
}

// Why an attach was closed (details of AttachClosed, and what the hub tells apart).
const (
	AttachCloseClient     = "client_closed"
	AttachCloseWorker     = "worker_closed"
	AttachCloseWorkerLost = "worker_offline"
	AttachCloseRevoked    = "identity_invalid"
	AttachCloseLease      = "lease_changed"
	AttachCloseRunEnded   = "run_ended"
	AttachCloseShutdown   = "controller_shutdown"
	AttachCloseSlowClient = "slow_client"
	AttachCloseSlowWorker = "slow_worker"
	AttachCloseProtocol   = "protocol_error"
)

// AttachOpenRequest is the body of POST /v1/runs/{ref}/attach/stream.
type AttachOpenRequest struct {
	Mode string `json:"mode"`
	// Hz is the rate of a screen stream; the Controller clamps it. Other modes ignore it.
	Hz int `json:"hz,omitempty"`
}

// AttachPeer is someone inside the session of a Run, as the Controller knows
// them from their certificate: never as they say they are.
type AttachPeer struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Mode string `json:"mode"`
}

// AttachRequest is what a Worker gets from the long-poll: one client that wants
// into a Run it holds. The Worker dials SessionID back before ExpiresAt.
type AttachRequest struct {
	SessionID  string        `json:"session_id"`
	RunID      string        `json:"run_id"`
	LeaseEpoch uint64        `json:"lease_epoch"`
	Mode       string        `json:"mode"`
	Hz         int           `json:"hz,omitempty"` // granted rate of a screen stream
	Handle     RuntimeHandle `json:"handle"`
	Client     AttachPeer    `json:"client"`
	ExpiresAt  time.Time     `json:"expires_at"`
}

// AttachRequestsResponse is GET /v1/worker/attach-requests: empty when the wait
// ended with nothing to do, and the Worker asks again.
type AttachRequestsResponse struct {
	Requests []AttachRequest `json:"requests"`
}

// FrameType says what a frame carries.
type FrameType byte

// Frame types. Data is the bytes of the terminal in either direction; resize
// is cols and rows (client to Worker); close ends the session in order; error
// ends it with a CF_* code; peers is the list of who is in the Run, and only the
// Controller sends it.
const (
	FrameData   FrameType = 1
	FrameResize FrameType = 2
	FrameClose  FrameType = 3
	FrameError  FrameType = 4
	FramePeers  FrameType = 5
)

// Framing: a type byte, the payload length as a big-endian uint32, the payload.
const (
	FrameHeaderBytes = 5
	// MaxFramePayload bounds one frame: a reader never holds more than this
	// for a peer that lies about the length.
	MaxFramePayload = 64 << 10
)

// ErrBadFrame is a frame that breaks the framing: an unknown type, a length
// over MaxFramePayload, or a payload that does not read as its type.
var ErrBadFrame = errors.New("v1: malformed attach frame")

// Frame is one unit of an attach stream.
type Frame struct {
	Type    FrameType
	Payload []byte
}

func (t FrameType) valid() bool { return t >= FrameData && t <= FramePeers }

// AppendFrame appends the wire form of f to dst.
func AppendFrame(dst []byte, f Frame) ([]byte, error) {
	if !f.Type.valid() {
		return dst, fmt.Errorf("%w: type %d", ErrBadFrame, f.Type)
	}
	if len(f.Payload) > MaxFramePayload {
		return dst, fmt.Errorf("%w: payload of %d bytes, at most %d", ErrBadFrame, len(f.Payload), MaxFramePayload)
	}
	dst = append(dst, byte(f.Type))
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(f.Payload))) //nolint:gosec // bounded by MaxFramePayload above
	return append(dst, f.Payload...), nil
}

// WriteFrame writes f in one Write.
func WriteFrame(w io.Writer, f Frame) error {
	buf, err := AppendFrame(make([]byte, 0, FrameHeaderBytes+len(f.Payload)), f)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame. It returns io.EOF when the stream ended between
// frames, io.ErrUnexpectedEOF inside one, and ErrBadFrame for a header that
// breaks the framing (before reading a payload it would not accept).
func ReadFrame(r io.Reader) (Frame, error) {
	var head [FrameHeaderBytes]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Frame{}, err
	}
	t := FrameType(head[0])
	if !t.valid() {
		return Frame{}, fmt.Errorf("%w: type %d", ErrBadFrame, head[0])
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > MaxFramePayload {
		return Frame{}, fmt.Errorf("%w: payload of %d bytes, at most %d", ErrBadFrame, n, MaxFramePayload)
	}
	f := Frame{Type: t}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	return f, nil
}

// DataFrame carries bytes of the terminal.
func DataFrame(p []byte) Frame { return Frame{Type: FrameData, Payload: p} }

// ResizeFrame says the terminal is now cols by rows.
func ResizeFrame(cols, rows uint16) Frame {
	return Frame{Type: FrameResize, Payload: binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, cols), rows)}
}

// ParseResize reads the payload of a resize frame.
func ParseResize(p []byte) (cols, rows uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("%w: resize of %d bytes", ErrBadFrame, len(p))
	}
	return binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:]), nil
}

// CloseFrame ends the session in order: code is the exit status of what ran
// (0 for a detach) and message a line for the person.
func CloseFrame(code uint32, message string) Frame {
	return Frame{Type: FrameClose, Payload: append(binary.BigEndian.AppendUint32(nil, code), clip(message)...)}
}

// ParseClose reads the payload of a close frame.
func ParseClose(p []byte) (code uint32, message string, err error) {
	if len(p) < 4 || !utf8.Valid(p[4:]) {
		return 0, "", fmt.Errorf("%w: close frame", ErrBadFrame)
	}
	return binary.BigEndian.Uint32(p), string(p[4:]), nil
}

// StreamFrameError is the payload of an error frame: the envelope of an error, as in a response.
type StreamFrameError struct {
	Code    domain.Code `json:"code"`
	Message string      `json:"message"`
}

// ErrorFrame ends the session with a CF_* code.
func ErrorFrame(code domain.Code, message string) Frame {
	raw, _ := json.Marshal(StreamFrameError{Code: code, Message: clip(message)}) // two strings cannot fail to marshal
	return Frame{Type: FrameError, Payload: raw}
}

// ParseError reads the payload of an error frame; a frame with no code is malformed.
func ParseError(p []byte) (StreamFrameError, error) {
	var e StreamFrameError
	if err := json.Unmarshal(p, &e); err != nil || e.Code == "" {
		return StreamFrameError{}, fmt.Errorf("%w: error frame", ErrBadFrame)
	}
	return e, nil
}

// MaxPeers is how many peers a peers frame lists; the rest are left out.
const MaxPeers = 64

// PeersFrame lists who is in the Run.
func PeersFrame(peers []AttachPeer) (Frame, error) {
	if len(peers) > MaxPeers {
		peers = peers[:MaxPeers]
	}
	raw, err := json.Marshal(peers)
	if err != nil {
		return Frame{}, fmt.Errorf("v1: encoding peers: %w", err)
	}
	if len(raw) > MaxFramePayload {
		return Frame{}, fmt.Errorf("%w: peers do not fit a frame", ErrBadFrame)
	}
	return Frame{Type: FramePeers, Payload: raw}, nil
}

// ParsePeers reads the payload of a peers frame.
func ParsePeers(p []byte) ([]AttachPeer, error) {
	var out []AttachPeer
	if err := json.Unmarshal(p, &out); err != nil {
		return nil, fmt.Errorf("%w: peers frame", ErrBadFrame)
	}
	return out, nil
}

// clip keeps a message for a person under a size that always fits a frame.
func clip(s string) string {
	const limit = 1 << 10
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
