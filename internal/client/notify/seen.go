package notify

import "time"

// seen remembers up to cap keys, forgetting the oldest first; with a ttl, a key
// also expires. Its memory is bounded whatever the stream does.
type seen struct {
	cap   int
	ttl   time.Duration // zero: keys do not expire
	at    map[string]time.Time
	order []string
}

func newSeen(capacity int, ttl time.Duration) *seen {
	return &seen{cap: capacity, ttl: ttl, at: map[string]time.Time{}}
}

// has says whether key was added and is still remembered at now.
func (s *seen) has(key string, now time.Time) bool {
	t, ok := s.at[key]
	return ok && (s.ttl == 0 || now.Sub(t) < s.ttl)
}

func (s *seen) add(key string, now time.Time) {
	if _, ok := s.at[key]; !ok {
		s.order = append(s.order, key)
		for len(s.order) > s.cap {
			delete(s.at, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.at[key] = now
}
