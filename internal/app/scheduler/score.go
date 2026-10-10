package scheduler

import (
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Weights of the score (§10.1). They are integers, never floats, so the same
// inputs score identically everywhere. Preference dominates everything else:
// it is the local-first of the spec, and the order of the prefer list ranks
// among the preferred.
const (
	weightPrefer   = 1_000_000
	weightFree     = 1000 // per resource, scaled by the fraction left free
	weightLoad     = 1000
	weightAffinity = 100
)

// Score explains a ranking: Total is the sum of the other parts.
type Score struct {
	Total    int64
	Prefer   int64
	Free     int64
	Load     int64
	Affinity int64
}

func score(req Request, w Worker) Score {
	var s Score
	if i := slices.Index(req.Prefer, w.Name); i >= 0 {
		s.Prefer = int64(weightPrefer * (len(req.Prefer) - i))
	}
	left := v1.Capacity{
		CPU:      w.Allocatable.CPU - w.Reserved.CPU - req.Resources.CPU,
		MemoryMB: w.Allocatable.MemoryMB - w.Reserved.MemoryMB - req.Resources.MemoryMB,
	}
	s.Free = fraction(left.CPU, w.Allocatable.CPU) + fraction(left.MemoryMB, w.Allocatable.MemoryMB)
	if w.MaxConcurrentRuns > 0 {
		s.Load = -int64(weightLoad * w.Reservations / w.MaxConcurrentRuns)
	}
	if w.KnowsProject {
		s.Affinity = weightAffinity
	}
	s.Total = s.Prefer + s.Free + s.Load + s.Affinity
	return s
}

// fraction is weightFree times left over of, in integers.
func fraction(left, of int) int64 {
	if of <= 0 {
		return 0
	}
	return int64(weightFree) * int64(left) / int64(of)
}
