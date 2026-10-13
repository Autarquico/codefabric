package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// OutputQuery selects output of a Run: Tail (the last bytes) or Offset (from a
// position) exclude each other; Epoch fixes the lease epoch, else the Run's
// latest; Limit bounds the bytes of a page.
type OutputQuery struct {
	Epoch  uint64
	Tail   *uint64
	Offset *uint64
	Limit  uint64
}

func (q OutputQuery) values() url.Values {
	v := url.Values{}
	if q.Epoch > 0 {
		v.Set("epoch", strconv.FormatUint(q.Epoch, 10))
	}
	if q.Tail != nil {
		v.Set("tail", strconv.FormatUint(*q.Tail, 10))
	}
	if q.Offset != nil {
		v.Set("offset", strconv.FormatUint(*q.Offset, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.FormatUint(q.Limit, 10))
	}
	return v
}

// RunOutput reads a page of the output of the Run the reference points to.
func (c *Client) RunOutput(ctx context.Context, ref string, q OutputQuery) (v1.RunOutput, error) {
	var out v1.RunOutput
	err := c.do(ctx, http.MethodGet, withQuery(refPath("runs", ref, "output"), q.values()), nil, &out)
	return out, err
}

// OutputHandler takes what a follow of output delivers. Begin comes first on
// every connection; Chunk for each stretch of bytes.
type OutputHandler struct {
	Begin func(v1.OutputBegin) error
	Chunk func(v1.OutputChunk) error
}

// OutputEnding says how a follow ended: exactly one is set. EOF, the output is
// closed and every byte was delivered; End, it will not be closed (the Run is
// over and quiet, or it went on under another lease epoch).
type OutputEnding struct {
	EOF *v1.OutputEOF
	End *v1.OutputEnd
}

// FollowOutput delivers the output of the Run from where q says, in the epoch
// fixed when it opens, until it is closed (EOF) or will not be (End), or ctx
// ends, or the handler fails. A broken or silent connection is reopened at the
// offset of the last chunk the handler took, in the same epoch.
func (c *Client) FollowOutput(ctx context.Context, ref string, q OutputQuery, h OutputHandler) (OutputEnding, error) {
	var ending OutputEnding
	q.Limit = 0
	path := withQuery(refPath("runs", ref, "output"), withFollow(q.values()))
	err := c.follow(ctx, streamSpec{
		request: func(lastID string) (string, http.Header) {
			header := http.Header{}
			if lastID != "" {
				header.Set("Last-Event-ID", lastID)
			}
			return path, header
		},
		handle: func(ev sseEvent) (bool, string, error) {
			switch ev.Name {
			case v1.SSEBegin:
				var b v1.OutputBegin
				if err := decodeEvent(ev, &b); err != nil {
					return false, "", err
				}
				return false, strconv.FormatUint(b.LeaseEpoch, 10) + ":" + strconv.FormatUint(b.Offset, 10), h.Begin(b)
			case v1.SSEOutput:
				var chunk v1.OutputChunk
				if err := decodeEvent(ev, &chunk); err != nil {
					return false, "", err
				}
				return false, "", h.Chunk(chunk)
			case v1.SSEEOF:
				var e v1.OutputEOF
				if err := decodeEvent(ev, &e); err != nil {
					return false, "", err
				}
				ending.EOF = &e
				return true, "", nil
			case v1.SSEEnd:
				var e v1.OutputEnd
				if err := decodeEvent(ev, &e); err != nil {
					return false, "", err
				}
				ending.End = &e
				return true, "", nil
			}
			return false, "", nil
		},
	})
	return ending, err
}

func withFollow(v url.Values) url.Values {
	v.Set("follow", "true")
	return v
}

func decodeEvent(ev sseEvent, into any) error {
	if err := json.Unmarshal([]byte(ev.Data), into); err != nil {
		return domain.NewError(domain.CodeInternal, "the Controller sent an unreadable "+ev.Name+" event", nil)
	}
	return nil
}

// UploadOutput sends the output of the Runs of this Worker (POST /v1/worker/output).
func (c *Client) UploadOutput(ctx context.Context, batch v1.OutputBatch) (v1.OutputBatchResponse, error) {
	var out v1.OutputBatchResponse
	err := c.do(ctx, http.MethodPost, "/v1/worker/output", batch, &out)
	return out, err
}
