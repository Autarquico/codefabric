package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

const (
	healthTimeout  = 2 * time.Second
	healthCacheTTL = 5 * time.Second
)

// Store is what the API needs from the Controller's database.
type Store interface {
	ControllerID() string
	Health(ctx context.Context) error
}

// healthCache keeps the last database check for healthCacheTTL. The mutex is
// held while a check runs, so callers that arrive meanwhile wait for that one
// result instead of each starting their own.
type healthCache struct {
	mu      sync.Mutex
	checked time.Time
	valid   bool
	err     error
}

func (h *healthCache) check(now time.Time, run func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.valid && now.Sub(h.checked) < healthCacheTTL {
		return h.err
	}
	err := run()
	h.checked, h.valid, h.err = now, true, err
	return err
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

	if err := s.health.check(now, func() error { return s.checkDatabase(r) }); err != nil {
		st.Database.Error = "database check failed"
	} else {
		st.Database.OK = true
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

// checkDatabase runs detached from the caller's cancellation: its result is
// shared with other callers, so one impatient client must not poison it.
func (s *Server) checkDatabase(r *http.Request) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.timeouts.Health)
	defer cancel()
	err := s.store.Health(ctx)
	if err != nil {
		s.log.ErrorContext(r.Context(), "database health check failed", "request_id", stateOf(r.Context()).id, "err", err)
	}
	return err
}
