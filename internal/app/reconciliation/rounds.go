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
	absent    int // leases served that the round did not name: it cannot complete
}

// rounds keeps the latest round of each Worker; a new round replaces it.
type rounds struct {
	mu       sync.Mutex
	byWorker map[domain.ID]*round
}

func newRounds() *rounds { return &rounds{byWorker: map[domain.ID]*round{}} }

// peek checks that fragment index follows the ones the Controller has of the
// round of the Worker (a fragment 0 starts a new round) and returns what the
// round named so far, without changing anything: advance does once the
// fragment is committed. false means the Worker starts the round again.
func (r *rounds) peek(worker domain.ID, epoch, id string, index, count int) (map[leaseKey]bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index == 0 {
		return map[leaseKey]bool{}, true
	}
	cur := r.byWorker[worker]
	if cur == nil || cur.epoch != epoch || cur.id != id || cur.count != count || cur.next != index {
		return nil, false
	}
	named := make(map[leaseKey]bool, len(cur.named))
	for k := range cur.named {
		named[k] = true
	}
	return named, true
}

// advance records a committed fragment and the leases it named.
func (r *rounds) advance(worker domain.ID, epoch, id string, index, count int, named []leaseKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.byWorker[worker]
	if index == 0 {
		cur = &round{epoch: epoch, id: id, count: count, named: map[leaseKey]bool{}}
		r.byWorker[worker] = cur
	}
	if cur == nil || cur.id != id || cur.next != index {
		return
	}
	for _, k := range named {
		cur.named[k] = true
	}
	cur.next++
}

// served records that a page of the answer listed absent leases.
func (r *rounds) served(worker domain.ID, id string, absent int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.byWorker[worker]; cur != nil && cur.id == id {
		cur.absent += absent
	}
}

// complete is the round of the Worker whose fragments all came, for the pages
// of its answer; false if there is none.
func (r *rounds) complete(worker domain.ID, epoch, id string) (named map[leaseKey]bool, absent int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.byWorker[worker]
	if cur == nil || cur.epoch != epoch || cur.id != id || cur.next != cur.count {
		return nil, 0, false
	}
	named = make(map[leaseKey]bool, len(cur.named))
	for k := range cur.named {
		named[k] = true
	}
	return named, cur.absent, true
}

// forget drops the round of the Worker once it is answered whole.
func (r *rounds) forget(worker domain.ID, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.byWorker[worker]; cur != nil && cur.id == id {
		delete(r.byWorker, worker)
	}
}
