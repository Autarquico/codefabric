package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func envelope(w http.ResponseWriter, status int, code domain.Code, details map[string]any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v1.ErrorEnvelope{Error: v1.ErrorBody{Code: code, Message: "no", Details: details}})
}

func TestWorkflowReads(t *testing.T) {
	var paths []string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		switch r.URL.Path {
		case "/v1/workflows":
			_ = json.NewEncoder(w).Encode(v1.ListWorkflowsResponse{Workflows: []v1.WorkflowSummary{{Name: "a", Head: 2}}})
		case "/v1/workflows/a@1":
			_ = json.NewEncoder(w).Encode(v1.WorkflowVersion{Workflow: "a", Version: 1})
		case "/v1/workflows/a/history":
			_ = json.NewEncoder(w).Encode(v1.WorkflowHistoryResponse{Versions: []v1.WorkflowVersion{{Version: 2}, {Version: 1}}})
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	ctx := context.Background()
	list, err := c.ListWorkflows(ctx)
	if err != nil || len(list) != 1 || list[0].Head != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	v, err := c.ShowWorkflow(ctx, "a@1")
	if err != nil || v.Version != 1 {
		t.Fatalf("show: %v %v", v, err)
	}
	h, err := c.WorkflowHistory(ctx, "a")
	if err != nil || len(h) != 2 || h[0].Version != 2 {
		t.Fatalf("history: %v %v", h, err)
	}
	if len(paths) != 3 {
		t.Fatalf("requests: %v", paths)
	}
}

func TestCommitWorkflow_SendsTheRequestIDAndDecodesAConflict(t *testing.T) {
	var got v1.CommitWorkflowRequest
	conflict := false
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workflows/a/commits" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if conflict {
			envelope(w, http.StatusConflict, domain.CodeWorkflowConflict, map[string]any{"head": 3})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(v1.WorkflowVersion{Workflow: "a", Version: 1})
	}))
	v, err := c.CommitWorkflow(context.Background(), "a", v1.CommitWorkflowRequest{RequestID: "req-1", Changelog: "x"})
	if err != nil || v.Version != 1 || got.RequestID != "req-1" || got.Changelog != "x" {
		t.Fatalf("commit: %v %v %+v", v, err, got)
	}
	conflict = true
	_, err = c.CommitWorkflow(context.Background(), "a", v1.CommitWorkflowRequest{Parent: 1})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeWorkflowConflict || de.Details["head"] != float64(3) {
		t.Fatalf("conflict: %#v", err)
	}
}

func TestHasBlob_PresentAndAbsent(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method %s", r.Method)
		}
		if r.URL.Path != "/v1/blobs/"+testDigest {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	if ok, err := c.HasBlob(context.Background(), testDigest); err != nil || !ok {
		t.Fatalf("present: %v %v", ok, err)
	}
	if ok, err := c.HasBlob(context.Background(), "sha256:other"); err != nil || ok {
		t.Fatalf("absent: %v %v", ok, err)
	}
}

func TestHasBlob_OtherRefusalsAreErrors(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	if ok, err := c.HasBlob(context.Background(), testDigest); err == nil || ok {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestPutBlob_StreamsABodyOverTheJSONCap(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 200<<10) // > the 64 KiB JSON cap
	var gotLen int64
	var gotBody []byte
	var gotType string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen, gotType = r.ContentLength, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(v1.BlobMeta{Digest: testDigest, Size: int64(len(gotBody))})
	}))
	meta, err := c.PutBlob(context.Background(), testDigest, bytes.NewReader(payload), int64(len(payload)))
	if err != nil || meta.Size != int64(len(payload)) {
		t.Fatalf("put: %v %v", meta, err)
	}
	if gotLen != int64(len(payload)) || !bytes.Equal(gotBody, payload) || gotType != "application/octet-stream" {
		t.Fatalf("server saw len=%d type=%q equal=%v", gotLen, gotType, bytes.Equal(gotBody, payload))
	}
}

func TestPutBlob_EmptyBody(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 {
			t.Errorf("content length %d", r.ContentLength)
		}
		_ = json.NewEncoder(w).Encode(v1.BlobMeta{Digest: testDigest})
	}))
	if _, err := c.PutBlob(context.Background(), testDigest, bytes.NewReader(nil), 0); err != nil {
		t.Fatal(err)
	}
}

func TestPutBlob_413DecodesIntoTheDomainError(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, http.StatusRequestEntityTooLarge, domain.CodeInvalidRequest, map[string]any{"max_bytes": 10})
	}))
	_, err := c.PutBlob(context.Background(), testDigest, bytes.NewReader([]byte("0123456789ab")), 12)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest || de.Details["max_bytes"] != float64(10) {
		t.Fatalf("got %#v", err)
	}
}

func TestOpenBlob_StreamsAndErrorsDecode(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 100<<10)
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/blobs/"+testDigest {
			envelope(w, http.StatusNotFound, domain.CodeNotFound, nil)
			return
		}
		w.Header().Set("Content-Length", "102400")
		_, _ = w.Write(payload)
	}))
	rc, size, err := c.OpenBlob(context.Background(), testDigest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if size != int64(len(payload)) || !bytes.Equal(got, payload) {
		t.Fatalf("size %d, %d bytes read", size, len(got))
	}
	if _, _, err := c.OpenBlob(context.Background(), "sha256:other"); !isCode(err, domain.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}
