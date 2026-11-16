package attachrelay

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// MaxScreenBytes bounds one screen: it is sent as one data frame, so it is what
// a frame holds. A screen over it ends the stream with an error frame instead of
// being cut in the middle of an escape sequence.
const MaxScreenBytes = v1.MaxFramePayload

// captureTimeout bounds one capture of the pane, on real time.
const captureTimeout = 2 * time.Second

// screenStream serves the mode "screen" of one attach: no pty and no tmux client,
// just the pane read at the granted rate. The whole screen is sent each time it
// changed, never a diff. The Controller connection may be slow: the capture loop
// never waits for it and keeps only the newest screen (latest wins), so a slow
// viewer sees fewer screens, not old ones. It ends when the client closes, when
// the Run is gone, and when the Worker shuts down; every goroutine is joined.
func (r *Relay) screenStream(ctx context.Context, conn Stream, req v1.AttachRequest) {
	handle := r.o.Runtime.HandleFor(domain.SessionName(req.RunID, req.LeaseEpoch))
	every := time.Second / time.Duration(v1.ClampScreenHz(req.Hz))

	stop := make(chan struct{})
	gone := make(chan struct{})     // the client closed, or the Controller went away
	connLost := make(chan struct{}) // writing to the Controller failed
	latest := make(chan []byte, 1)  // the newest screen not yet written
	var wg sync.WaitGroup
	defer func() { // the stream first, which unblocks the reader and the writer
		_ = conn.Close()
		close(stop)
		wg.Wait()
	}()

	wg.Add(2)
	go func() { // what the client says: only that it is done matters
		defer wg.Done()
		defer close(gone)
		for {
			f, err := conn.ReadFrame()
			if err != nil || f.Type == v1.FrameClose || f.Type == v1.FrameError {
				return
			}
		}
	}()
	go func() { // the newest screen into the stream
		defer wg.Done()
		for {
			select {
			case b := <-latest:
				if err := conn.WriteFrame(v1.DataFrame(b)); err != nil {
					close(connLost)
					return
				}
			case <-stop:
				return
			}
		}
	}()

	tick := r.o.Clock.NewTicker(every)
	defer tick.Stop()
	var last []byte
	first := true
	for {
		if !first {
			select {
			case <-tick.C():
			case <-gone:
				return
			case <-connLost:
				return
			case <-ctx.Done():
				r.farewell(conn, v1.ErrorFrame(domain.CodeWorkerOffline, "the Worker is shutting down"))
				return
			}
		}
		first = false
		cctx, cancel := context.WithTimeout(ctx, captureTimeout)
		screen, err := r.o.Runtime.Snapshot(cctx, handle)
		cancel()
		if ctx.Err() != nil {
			r.farewell(conn, v1.ErrorFrame(domain.CodeWorkerOffline, "the Worker is shutting down"))
			return
		}
		if err != nil {
			if end := r.screenEnded(ctx, handle, err); end != nil {
				r.farewell(conn, v1.ErrorFrame(end.Code, end.Message))
				return
			}
			continue // a failed capture is one screen missed, not the end
		}
		if len(screen) > MaxScreenBytes {
			r.o.Log.WarnContext(ctx, "attach relay: the screen is too big; ending the stream", "run", req.RunID, "bytes", len(screen))
			r.farewell(conn, v1.ErrorFrame(domain.CodeInvalidRequest, "the screen of this Run is too big to stream"))
			return
		}
		if last != nil && bytes.Equal(screen, last) {
			continue // nothing changed: nothing is sent
		}
		last = screen
		select { // latest wins: what was waiting for the writer is stale now
		case <-latest:
		default:
		}
		latest <- screen // the only producer, and the slot was just emptied or never full
	}
}

// screenEnded says whether a failed capture means the stream is over, and with
// which error: a coded error of the Runtime (the Run is not there, or the
// Runtime cannot show screens) ends it, and so does a session that is not alive.
func (r *Relay) screenEnded(ctx context.Context, h cfruntime.Handle, err error) *domain.Error {
	var coded *domain.Error
	if errors.As(err, &coded) {
		return coded
	}
	sctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	if st, serr := r.o.Runtime.Status(sctx, h); serr == nil && !st.Alive {
		return domain.NewError(domain.CodeRunNotAttachable, "the Run is not running on this Worker",
			map[string]any{"reason": v1.NotAttachableNotRunning})
	}
	r.o.Log.WarnContext(ctx, "attach relay: capturing the screen failed", "err", err)
	return nil
}
