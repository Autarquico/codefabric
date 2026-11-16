package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// ScreenStream is a read-only stream of the screen of a Run. Each screen is the
// whole screen, not a change to the last one, so a reader that falls behind
// loses nothing it needs: it only has to read the latest. The bytes are what the
// Worker captured, SGR included, and are NOT sanitized: draw them through
// internal/support/screen.Sanitize.
type ScreenStream struct {
	conn *AttachConn
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
// has already arrived, the older ones are dropped. The stream ending is a *domain.Error
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
		case v1.FrameError:
			return nil, StreamError(f)
		case v1.FrameClose:
			return nil, io.EOF
		}
		if latest != nil && s.conn.r.Buffered() < v1.FrameHeaderBytes {
			return latest, nil // nothing more waiting, whatever the last frame was: this is the newest
		}
	}
}

// Close ends the stream.
func (s *ScreenStream) Close() error { return s.conn.Close() }
