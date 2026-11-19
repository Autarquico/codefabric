package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
)

// ScreenStream is a read-only stream of the screen of a Run. Each screen is the
// whole screen, not a change to the last one, so a reader that falls behind
// loses nothing it needs: it only has to read the latest. The bytes are what the
// Worker captured, SGR included, and are NOT sanitized: draw them through
// internal/support/screen.Sanitize.
type ScreenStream struct {
	conn *AttachConn

	last []byte // the newest screen Next returned; only Next touches it

	mu    sync.Mutex
	peers []v1.AttachPeer
}

// Peers is who the Controller last said is inside the Run (the writers and the
// viewers of its session; screen streams are never listed). It is empty until
// the Controller says, and it may be read from another goroutine than Next's.
func (s *ScreenStream) Peers() []v1.AttachPeer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]v1.AttachPeer(nil), s.peers...)
}

// OpenScreen opens the screen stream of the Run of ref (an ID, a short ID or a
// Task) at hz screens a second; the Controller clamps it to v1.ScreenMinHz..
// v1.ScreenMaxHz and 0 asks for v1.ScreenDefaultHz. The errors are those of
// OpenAttach. The caller closes the stream; ctx bounds only the opening.
func (c *Client) OpenScreen(ctx context.Context, ref, project string, hz int) (*ScreenStream, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	body, err := json.Marshal(v1.AttachOpenRequest{Mode: v1.AttachModeScreen, Hz: hz})
	if err != nil {
		return nil, fmt.Errorf("client: encoding request: %w", err)
	}
	conn, err := c.upgrade(ctx, http.MethodPost, withQuery(refPath("runs", ref, "attach/stream"), q), body)
	if err != nil {
		return nil, err
	}
	return &ScreenStream{conn: conn}, nil
}

// Next waits for the next screen and returns the latest one: if more than one
// has already arrived, the older ones are dropped. When the Controller says who
// is inside the Run changed and no screen follows (the Run is idle), the last
// screen is returned again, so that a reader that looks at Peers after each
// Next learns of it. The stream ending is a *domain.Error
// with the code the Controller or the Worker gave (CF_RUN_NOT_ATTACHABLE when the
// Run ended, CF_WORKER_OFFLINE...), io.EOF if it simply closed, and a close frame
// from the Worker is io.EOF too.
func (s *ScreenStream) Next() ([]byte, error) {
	var latest []byte
	for {
		f, err := s.conn.ReadFrame()
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case v1.FrameData:
			latest = f.Payload
		case v1.FramePeers:
			if peers, err := v1.ParsePeers(f.Payload); err == nil && s.setPeers(peers) && latest == nil && s.last != nil {
				// Someone came or went and the Run is idle, so no screen is coming
				// to carry the news: the last one is returned again, and Peers is new.
				return s.last, nil
			}
		case v1.FrameError:
			return nil, StreamError(f)
		case v1.FrameClose:
			return nil, io.EOF
		}
		if latest != nil && s.conn.r.Buffered() < v1.FrameHeaderBytes {
			s.last = latest
			return latest, nil // nothing more waiting, whatever the last frame was: this is the newest
		}
	}
}

// setPeers records who is inside and says whether it changed.
func (s *ScreenStream) setPeers(peers []v1.AttachPeer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := !slices.Equal(s.peers, peers)
	s.peers = peers
	return changed
}

// Close ends the stream.
func (s *ScreenStream) Close() error { return s.conn.Close() }
