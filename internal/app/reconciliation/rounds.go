package reconciliation

import (
	"sync"

	"github.com/Autarquico/codefabric/internal/domain"
)

// leaseKey names a Run under one lease epoch.
type leaseKey struct {
	run   domain.ID
	epoch uint64
}

// round is what the Controller remembers of a round of reconciliation sent in
// fragments: which leases its fragments named or resolved so far. It lives in
// memory: a Controller that restarts mid-round asks the Worker to start again.
type round struct {
	epoch, id string
	count     int
	next      int // the index of the fragment expected next; count once all came
	named     map[leaseKey]bool
}

// rounds keeps the latest round of each Worker; a new round replaces it.
type rounds struct {
	mu       sync.Mutex
	byWorker map[domain.ID]*round
}

func newRounds() *rounds { return &rounds{byWorker: map[domain.ID]*round{}} }

// fragment takes fragment index of the round of the Worker and the leases it
// names. It returns the round as it stands, and false if the fragment does not
// follow the ones the Controller has (out of order, or the round is unknown),
// in which case the Worker starts the round again.
func (r *rounds) fragment(worker domain.ID, epoch, id string, index, count int, named []leaseKey) (*round, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.byWorker[worker]
	if index == 0 {
		cur = &round{epoch: epoch, id: id, count: count, named: map[leaseKey]bool{}}
		r.byWorker[worker] = cur
	}
	if cur == nil || cur.epoch != epoch || cur.id != id || cur.count != count || cur.next != index {
		return nil, false
	}
	for _, k := range named {
		cur.named[k] = true
	}
	cur.next++
	return cur, true
}

// complete is the round of the Worker whose fragments all came, for the pages
// of its answer; false if there is none.
func (r *rounds) complete(worker domain.ID, epoch, id string) (*round, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.byWorker[worker]
	if cur == nil || cur.epoch != epoch || cur.id != id || cur.next != cur.count {
		return nil, false
	}
	return cur, true
}

// forget drops the round of the Worker once it is answered whole.
func (r *rounds) forget(worker domain.ID, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.byWorker[worker]; cur != nil && cur.id == id {
		delete(r.byWorker, worker)
	}
}
