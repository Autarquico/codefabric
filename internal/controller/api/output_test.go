package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func (f *schedulingFixture) acceptedRun(t *testing.T) {
	t.Helper()
	_, err := f.alpha.AcceptLease(t.Context(), f.assign.LeaseID, f.assign.Epoch)
	must(t, err)
}

func (f *schedulingFixture) upload(t *testing.T, offset uint64, text string) v1.OutputAck {
	t.Helper()
	body, err := json.Marshal(v1.OutputBatch{Entries: []v1.OutputEntry{{RunID: f.run.ID, LeaseEpoch: 1, Offset: offset, Data: []byte(text)}}})
	must(t, err)
	status, env, raw := f.call(f.httpClientFor(f.alphaD), http.MethodPost, "/v1/worker/output", string(body))
	if status != http.StatusOK {
		t.Fatalf("upload = %d %+v", status, env)
	}
	var out v1.OutputBatchResponse
	must(t, json.Unmarshal(raw, &out))
	return out.Acks[0]
}

// The authorisation matrix of the output routes.
func TestOutputRoutes_WhoMayCallWhat(t *testing.T) {
	f := newSchedulingFixture(t)
	f.acceptedRun(t)
	body := `{"entries":[]}`
	if status, env, _ := f.call(f.httpClientFor(f.ownerDir), http.MethodPost, "/v1/worker/output", body); status != http.StatusForbidden || env.Code != domain.CodePolicyDenied {
		t.Errorf("a client uploading: %d %s", status, env.Code)
	}
	if status, env, _ := f.call(f.httpClient(), http.MethodPost, "/v1/worker/output", body); status != http.StatusUnauthorized || env.Code != domain.CodeUnauthenticated {
		t.Errorf("anonymous upload: %d %s", status, env.Code)
	}
	for _, path := range []string{"/v1/runs/" + f.run.ID + "/output", "/v1/runs/" + f.run.ID + "/output?follow=true"} {
		if status, env, _ := f.call(f.httpClientFor(f.alphaD), http.MethodGet, path, ""); status != http.StatusForbidden || env.Code != domain.CodePolicyDenied {
			t.Errorf("a Worker reading %s: %d %s", path, status, env.Code)
		}
	}
	// A Worker without the lease gets a refusal per entry, no data.
	other := f.httpClientFor(f.betaD)
	raw, _ := json.Marshal(v1.OutputBatch{Entries: []v1.OutputEntry{{RunID: f.run.ID, LeaseEpoch: 1, Data: []byte("x")}}})
	status, _, got := f.call(other, http.MethodPost, "/v1/worker/output", string(raw))
	var out v1.OutputBatchResponse
	must(t, json.Unmarshal(got, &out))
	if status != 200 || out.Acks[0].Code != domain.CodeNotFound || out.Acks[0].End != 0 {
		t.Errorf("a foreign lease: %d %+v", status, out)
	}
	// A body past the limit, or with unknown fields, is refused whole.
	huge := `{"entries":[{"run_id":"` + strings.Repeat("a", v1.OutputBatchBytes) + `"}]}`
	if status, env, _ := f.call(f.httpClientFor(f.alphaD), http.MethodPost, "/v1/worker/output", huge); status != http.StatusBadRequest || env.Code != domain.CodeInvalidRequest {
		t.Errorf("huge body: %d %s", status, env.Code)
	}
	if status, env, _ := f.call(f.httpClientFor(f.alphaD), http.MethodPost, "/v1/worker/output", `{"entries":[],"x":1}`); status != http.StatusBadRequest || env.Code != domain.CodeInvalidRequest {
		t.Errorf("unknown field: %d %s", status, env.Code)
	}
}

func TestOutputRoutes_ReadAndParameters(t *testing.T) {
	f := newSchedulingFixture(t)
	f.acceptedRun(t)
	if ack := f.upload(t, 0, "hello world"); ack.End != 11 {
		t.Fatalf("ack = %+v", ack)
	}
	hc := f.httpClientFor(f.ownerDir)
	read := func(q string) v1.RunOutput {
		status, env, raw := f.call(hc, http.MethodGet, "/v1/runs/"+f.run.Short+"/output"+q, "")
		if status != 200 {
			t.Fatalf("%s: %d %+v", q, status, env)
		}
		var out v1.RunOutput
		must(t, json.Unmarshal(raw, &out))
		return out
	}
	if out := read("?tail=5"); string(out.Data) != "world" || out.Offset != 6 || out.End != 11 {
		t.Fatalf("tail = %+v", out)
	}
	if out := read("?offset=2&limit=3"); string(out.Data) != "llo" {
		t.Fatalf("offset = %+v", out)
	}
	for _, q := range []string{"?tail=1&offset=1", "?tail=x", "?follow=maybe", "?limit=999999", "?offset=99999999999999999999"} {
		if status, env, _ := f.call(hc, http.MethodGet, "/v1/runs/"+f.run.ID+"/output"+q, ""); status != 400 || env.Code != domain.CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, status, env.Code)
		}
	}
	if status, env, _ := f.call(hc, http.MethodGet, "/v1/runs/run-99/output", ""); status != 404 || env.Code != domain.CodeNotFound {
		t.Errorf("unknown run: %d %s", status, env.Code)
	}
}

func TestOutputRoutes_FollowIsFixedToItsEpochAndResumes(t *testing.T) {
	f := newSchedulingFixture(t)
	f.acceptedRun(t)
	f.upload(t, 0, "one ")
	hc := f.httpClientFor(f.ownerDir)

	s := f.mustStream(hc, "/v1/runs/"+f.run.ID+"/output?follow=true", nil)
	s.next(t) // retry
	var begin v1.OutputBegin
	if fr := s.nextEvent(t); fr.Event != v1.SSEBegin || json.Unmarshal([]byte(fr.Data), &begin) != nil || begin.LeaseEpoch != 1 || begin.End != 4 {
		t.Fatalf("begin = %+v", fr)
	}
	read := func(fr sseFrame) (v1.OutputChunk, string) {
		var c v1.OutputChunk
		if fr.Event != v1.SSEOutput || json.Unmarshal([]byte(fr.Data), &c) != nil {
			t.Fatalf("not an output frame: %+v", fr)
		}
		return c, fr.ID
	}
	c, id := read(s.nextEvent(t))
	if string(c.Data) != "one " || id != "1:4" || c.Offset != 0 {
		t.Fatalf("chunk = %+v id %s", c, id)
	}
	f.upload(t, 4, "two ")
	c, id = read(s.nextEvent(t))
	if string(c.Data) != "two " || id != "1:8" {
		t.Fatalf("chunk = %+v id %s", c, id)
	}
	s.cancel()

	// Reconnect with the id: only what came after, no duplicates.
	f.upload(t, 8, "three")
	again := f.mustStream(hc, "/v1/runs/"+f.run.ID+"/output?follow=true", map[string][]string{"Last-Event-Id": {"1:8"}})
	again.next(t)
	if fr := again.nextEvent(t); fr.Event != v1.SSEBegin {
		t.Fatalf("begin = %+v", fr)
	}
	c, id = read(again.nextEvent(t))
	if string(c.Data) != "three" || id != "1:13" {
		t.Fatalf("resumed chunk = %+v id %s", c, id)
	}
	// Close it: eof ends the stream.
	body, _ := json.Marshal(v1.OutputBatch{Entries: []v1.OutputEntry{{RunID: f.run.ID, LeaseEpoch: 1, EOF: true, FinalOffset: ptr(uint64(13))}}})
	f.call(f.httpClientFor(f.alphaD), http.MethodPost, "/v1/worker/output", string(body))
	if fr := again.nextEvent(t); fr.Event != v1.SSEEOF || !strings.Contains(fr.Data, `"final_offset":13`) {
		t.Fatalf("eof = %+v", fr)
	}
	if !again.ended(5e9) {
		t.Fatal("the stream did not end after eof")
	}
}

func TestOutputRoutes_FollowCursorErrors(t *testing.T) {
	f := newSchedulingFixture(t)
	f.acceptedRun(t)
	f.upload(t, 0, "abc")
	hc := f.httpClientFor(f.ownerDir)
	for name, id := range map[string]string{"no colon": "5", "epoch zero": "0:1", "text": "a:b", "negative": "1:-1"} {
		_, status, env := f.openStream(hc, "/v1/runs/"+f.run.ID+"/output?follow=true", map[string][]string{"Last-Event-Id": {id}})
		if status != 400 || env.Code != domain.CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, status, env.Code)
		}
	}
	_, status, env := f.openStream(hc, "/v1/runs/"+f.run.ID+"/output?follow=true", map[string][]string{"Last-Event-Id": {"1:99"}})
	if status != 409 || env.Code != domain.CodeCursorExpired {
		t.Errorf("offset past the end: %d %s", status, env.Code)
	}
}

func ptr[T any](v T) *T { return &v }
