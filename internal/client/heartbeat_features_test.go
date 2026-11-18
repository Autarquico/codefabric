package client

import (
	"net/http"
	"slices"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// A Worker reads the answer of a heartbeat as it reads any other: a field it
// does not know (a feature of a Controller newer than it) changes nothing, and
// one it knows arrives.
func TestHeartbeat_TheAnswerToleratesWhatItDoesNotKnowAndReadsTheFeatures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/worker/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"online","max_concurrent_runs":2,"features":["run_answer","from_the_future"],"a_field_of_tomorrow":{"x":1}}`))
	})
	got, err := testClient(t, mux).Heartbeat(t.Context(), v1.HeartbeatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrentRuns != 2 || !slices.Contains(got.Features, v1.FeatureRunAnswer) {
		t.Errorf("response = %+v", got)
	}
}
