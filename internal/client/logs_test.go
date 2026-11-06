package client

import (
	"errors"
	"net/http"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestShipLogs_AControllerWithoutTheRouteIsToldFromOneThatRefuses(t *testing.T) {
	old := http.NewServeMux() // a Controller older than the route: the mux answers in plain text
	if _, err := testClient(t, old).ShipLogs(t.Context(), v1.LogBatch{}); !errors.Is(err, ErrUnknownRoute) {
		t.Fatalf("a plain 404: %v", err)
	}
	var coded *domain.Error
	if _, err := testClient(t, old).ShipLogs(t.Context(), v1.LogBatch{}); !errors.As(err, &coded) || coded.Code != domain.CodeInternal {
		t.Fatalf("a plain 404 is still a CF_INTERNAL: %v", err)
	}
	wrongMethod := http.NewServeMux()
	wrongMethod.HandleFunc("GET /v1/worker/logs", func(http.ResponseWriter, *http.Request) {})
	if _, err := testClient(t, wrongMethod).ShipLogs(t.Context(), v1.LogBatch{}); !errors.Is(err, ErrUnknownRoute) {
		t.Fatalf("a plain 405: %v", err)
	}
	enveloped := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"CF_NOT_FOUND","message":"no such worker"}}`))
	})
	if _, err := testClient(t, enveloped).ShipLogs(t.Context(), v1.LogBatch{}); errors.Is(err, ErrUnknownRoute) || !errors.As(err, &coded) || coded.Code != domain.CodeNotFound {
		t.Fatalf("a 404 with an envelope is the Controller's answer: %v", err)
	}
}
