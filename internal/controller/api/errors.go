package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

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
		log.ErrorContext(r.Context(), "request failed", "request_id", stateOf(r.Context()).id, "method", r.Method, "path", r.URL.Path, "err", err)
		de = internalError(r)
	}
	status, ok := statusByCode[de.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, v1.ErrorEnvelope{Error: v1.ErrorBody{Code: de.Code, Message: de.Message, Details: de.Details}})
}

// fallbackBody answers when a body cannot even be serialised.
const fallbackBody = `{"error":{"code":"CF_INTERNAL","message":"internal error"}}` + "\n"

// writeJSON serialises body completely before anything is sent, and declares
// its length. A failure while building the response therefore never leaves a
// half-written 2xx, and a peer that frames the body by length (HTTP/1.0
// included) can tell a complete response from a cut one.
func writeJSON(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(fallbackBody)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
