package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

var statusByCode = map[domain.Code]int{
	domain.CodeInvalidRequest:    http.StatusBadRequest,
	domain.CodeUnauthenticated:   http.StatusUnauthorized,
	domain.CodePolicyDenied:      http.StatusForbidden,
	domain.CodeNotFound:          http.StatusNotFound,
	domain.CodeInvalidTransition: http.StatusConflict,
	domain.CodeNotImplemented:    http.StatusNotImplemented,
}

// writeError renders err as the {error:{code,message,details}} envelope. Only
// coded domain errors reach the caller verbatim: anything else is logged here
// and reported as a generic internal error, so no internals leak.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var de *domain.Error
	if !errors.As(err, &de) {
		log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		de = domain.NewError(domain.CodeInternal, "internal error", nil)
	}
	status, ok := statusByCode[de.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, v1.ErrorEnvelope{Error: v1.ErrorBody{Code: de.Code, Message: de.Message, Details: de.Details}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
