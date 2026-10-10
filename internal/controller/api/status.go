package api

import (
	"context"
	"net/http"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

const healthTimeout = 2 * time.Second

// Store is what the API needs from the Controller's database.
type Store interface {
	ControllerID() string
	Health(ctx context.Context) error
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ caller) error {
	now := s.clock.Now()
	st := v1.Status{
		ControllerID:  s.store.ControllerID(),
		Version:       s.build.Version,
		Commit:        s.build.Commit,
		StartedAt:     s.startedAt,
		UptimeSeconds: max(0, int64(now.Sub(s.startedAt)/time.Second)),
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := s.store.Health(ctx); err != nil {
		s.log.ErrorContext(r.Context(), "database health check failed", "request_id", stateOf(r.Context()).id, "err", err)
		st.Database.Error = "database check failed"
	} else {
		st.Database.OK = true
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}
