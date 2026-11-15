package api

import (
	"net/http"

	"github.com/Autarquico/codefabric/internal/controller/attachhub"
	"github.com/Autarquico/codefabric/internal/domain"
)

// AddRoute registers a route for a test, with the same authorisation as any other.
func (s *Server) AddRoute(pattern string, allow []domain.Kind, h func(http.ResponseWriter, *http.Request) error) {
	s.handle(pattern, allow, func(w http.ResponseWriter, r *http.Request, _ caller) error { return h(w, r) })
}

// AttachHub is the relay of attach, for tests to look into.
func (s *Server) AttachHub() *attachhub.Hub { return s.attach }
