package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/oklog/ulid/v2"

	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	requestIDHeader = "X-Request-ID"
	maxRequestIDLen = 64
)

// requestState is what the middlewares share about one request. The request ID
// middleware creates it before anything else runs, so the log line, the
// recovery and the error bodies all report the same ID; authentication fills
// in the principal on the same value.
type requestState struct {
	id        string
	principal domain.ID
	status    int // 0 until the response starts
	aborted   bool
}

type stateKey struct{}

func stateOf(ctx context.Context) *requestState {
	if st, ok := ctx.Value(stateKey{}).(*requestState); ok {
		return st
	}
	return &requestState{}
}

// responseWriter records how far the response got, which decides whether a
// panic can still be answered with an error body.
type responseWriter struct {
	http.ResponseWriter
	state *requestState
}

func (w *responseWriter) WriteHeader(status int) {
	if w.state.status == 0 {
		w.state.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.state.status == 0 {
		w.state.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach Flush and friends.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// middleware order, outermost first: request ID, log, recover, then the
// authorising handler. The log therefore sees the 500 that recover writes.
func (s *Server) chain(h http.Handler) http.Handler {
	return s.withRequestID(s.withLog(s.withRecover(h)))
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := &requestState{id: requestID(r)}
		w.Header().Set(requestIDHeader, st.id)
		next.ServeHTTP(&responseWriter{ResponseWriter: w, state: st},
			r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
	})
}

// requestID takes the caller's ID when it is a single, short, plain token;
// anything else (empty, long, repeated, with spaces or control characters) is
// replaced, so a header can never inject into the logs or the response.
func requestID(r *http.Request) string {
	if values := r.Header.Values(requestIDHeader); len(values) == 1 && validRequestID(values[0]) {
		return values[0]
	}
	return ulid.Make().String()
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// withLog writes one line per request: never bodies, query or headers.
func (s *Server) withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.clock.Now()
		defer func() {
			st := stateOf(r.Context())
			level := slog.LevelInfo
			if st.aborted || st.status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", st.status),
				slog.Duration("duration", s.clock.Now().Sub(start)),
				slog.String("request_id", st.id),
			}
			if st.principal != "" {
				attrs = append(attrs, slog.String("principal", string(st.principal)))
			}
			if st.aborted {
				attrs = append(attrs, slog.Bool("aborted", true))
			}
			s.log.LogAttrs(r.Context(), level, "request", attrs...)
		}()
		next.ServeHTTP(w, r)
	})
}

// withRecover turns a panic into a generic 500. If the response had already
// started it cannot be corrected, and finishing it normally would hand the
// caller a truncated body as if it were complete: the connection is aborted
// instead. The stack goes to the log only.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // net/http compares by identity
				panic(rec)
			}
			st := stateOf(r.Context())
			s.log.ErrorContext(r.Context(), "panic serving request",
				"request_id", st.id, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			if st.status != 0 {
				st.aborted = true
				panic(http.ErrAbortHandler)
			}
			writeError(w, r, s.log, internalError(r))
		}()
		next.ServeHTTP(w, r)
	})
}

// internalError is what every caller gets for a failure that is none of its
// business. The request ID is the handle to find the detail in the server log.
func internalError(r *http.Request) *domain.Error {
	return domain.NewError(domain.CodeInternal, "internal error", map[string]any{"request_id": stateOf(r.Context()).id})
}
