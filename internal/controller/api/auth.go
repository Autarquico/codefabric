package api

import (
	"context"
	"net/http"

	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

// route says who may call a pattern. An empty allow list means anonymous.
type route struct {
	allow   []domain.Kind
	handler handlerFunc
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, c caller) error

// caller is the authenticated side of a request.
// Its Session's Superseded marks a certificate that a renewal replaced: within
// its grace it may call the renewal endpoint and nothing else.
type caller struct{ pki.Session }

type callerKey struct{}

var (
	clientsOnly = []domain.Kind{domain.KindClient}
	workersOnly = []domain.Kind{domain.KindWorker}
	anyIdentity = []domain.Kind{domain.KindClient, domain.KindWorker}
)

func (s *Server) handle(pattern string, allow []domain.Kind, h handlerFunc) {
	s.routes[pattern] = route{allow: allow, handler: h}
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Context().Value(callerKey{}).(caller)
		if err := h(w, r, c); err != nil {
			writeError(w, r, s.log, err)
		}
	})
}

// serveHTTP is deny-by-default: a request is served only if its pattern is in
// the route table and its caller's kind is listed for it. Unknown routes get
// 401 without a certificate and 403 with one, so they reveal nothing. The
// identity is looked up on every request, which is what makes a revocation cut
// connections that are already open.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := s.mux.Handler(r)
	rt, known := s.routes[pattern]
	if known && len(rt.allow) == 0 {
		s.mux.ServeHTTP(w, r)
		return
	}

	c, err := s.authenticate(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	stateOf(r.Context()).principal = c.Principal.ID
	if !known || !contains(rt.allow, c.Principal.Kind) {
		writeError(w, r, s.log, domain.NewError(domain.CodePolicyDenied, "this identity may not call this endpoint", nil))
		return
	}
	if c.Superseded && pattern != renewPattern {
		writeError(w, r, s.log, domain.NewError(domain.CodeUnauthenticated, "certificate is not valid for this Controller", nil))
		return
	}
	s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
}

func (s *Server) authenticate(r *http.Request) (caller, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return caller{}, domain.NewError(domain.CodeUnauthenticated, "a client certificate is required", nil)
	}
	leaf := r.TLS.PeerCertificates[0]
	session, err := s.authority.Authenticate(r.Context(), leaf)
	if err != nil {
		return caller{}, err
	}
	return caller{session}, nil
}

func contains(kinds []domain.Kind, k domain.Kind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}
