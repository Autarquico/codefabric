package domain

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Kind is the entity type that prefixes an ID.
type Kind string

// Entity kinds.
const (
	KindProject      Kind = "project"
	KindPlan         Kind = "plan"
	KindTask         Kind = "task"
	KindRun          Kind = "run"
	KindWorker       Kind = "worker"
	KindWorkflow     Kind = "workflow"
	KindLease        Kind = "lease"
	KindCheckpoint   Kind = "cp"
	KindArtifact     Kind = "artifact"
	KindEvent        Kind = "event"
	KindConfirmation Kind = "confirmation"
	KindOperation    Kind = "operation"
	KindClient       Kind = "client"
)

var knownKinds = map[Kind]bool{
	KindProject: true, KindPlan: true, KindTask: true, KindRun: true,
	KindWorker: true, KindWorkflow: true, KindLease: true, KindCheckpoint: true,
	KindArtifact: true, KindEvent: true, KindConfirmation: true, KindOperation: true,
	KindClient: true,
}

// ErrMalformedID is returned for text that is not a canonical ID or short ID.
// §40 has no code for it: the edge validates input, the domain only reports.
var ErrMalformedID = errors.New("malformed identifier")

// ID is the canonical identifier "<kind>_<ULID>" (§3.13).
type ID string

func (id ID) String() string { return string(id) }

// ulidPart is the text after the first underscore, or "" if there is none.
func (id ID) ulidPart() string {
	_, rest, _ := strings.Cut(string(id), "_")
	return rest
}

// IDGenerator issues IDs that sort by creation time, even within one millisecond.
type IDGenerator struct {
	clock func() time.Time

	mu      sync.Mutex
	entropy ulid.MonotonicReader
}

// NewIDGenerator takes the clock and entropy source so tests can fix both.
func NewIDGenerator(clock func() time.Time, entropy io.Reader) *IDGenerator {
	return &IDGenerator{clock: clock, entropy: ulid.Monotonic(entropy, 0)}
}

// New panics if the entropy source fails: no caller can recover from that.
func (g *IDGenerator) New(kind Kind) ID {
	g.mu.Lock()
	defer g.mu.Unlock()

	id, err := ulid.New(ulid.Timestamp(g.clock()), g.entropy)
	if err != nil {
		panic(fmt.Sprintf("domain: generating %s id: %v", kind, err))
	}
	return ID(string(kind) + "_" + id.String())
}

// ParseID splits a canonical ID into its kind and ULID.
func ParseID(s string) (Kind, ulid.ULID, error) {
	kindText, ulidText, found := strings.Cut(s, "_")
	kind := Kind(kindText)
	if !found || !knownKinds[kind] {
		return "", ulid.ULID{}, fmt.Errorf("%w: %q", ErrMalformedID, s)
	}
	parsed, err := ulid.ParseStrict(ulidText)
	if err != nil {
		return "", ulid.ULID{}, fmt.Errorf("%w: %q: %w", ErrMalformedID, s, err)
	}
	return kind, parsed, nil
}
