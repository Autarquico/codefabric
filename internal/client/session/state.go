package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// State is what this machine remembers of the sessions it handed over: for each
// session, the Task it is bound to (the registry `session -> task`) and whether it was
// handed off (the `handed-off` marker). It lives in the config dir of cf, one file per
// session, so that the Controller never knows it.
type State struct{ dir string }

// NewState is the state kept under dir, which is made when something is written.
func NewState(dir string) State { return State{dir: dir} }

// Record is what is remembered of one session.
type Record struct {
	// Task is the Task the session was seeded into ("" if it never was).
	Task string `json:"task,omitempty"`
	// Tree is the tree of the snapshot that went with the last handoff ("" if none did):
	// the last state of the files the Controller was told about.
	Tree string `json:"tree,omitempty"`
	// HandedOff is set while the work of the session continues somewhere else: writing
	// in the local session now would make two writers.
	HandedOff *HandedOff `json:"handed_off,omitempty"`
}

// HandedOff is the marker of a session that was handed over.
type HandedOff struct {
	Task string `json:"task"`
	// Digest is that of the seed that was uploaded: what tells a Run seeded from this
	// machine from one seeded from another.
	Digest string    `json:"digest"`
	At     time.Time `json:"at"`
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)

func (s State) path(id string) (string, error) {
	if !safeID.MatchString(id) {
		return "", fmt.Errorf("session: %q is not a session id", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Get is what is remembered of the session; the zero Record if nothing is.
func (s State) Get(id string) (Record, error) {
	p, err := s.path(id)
	if err != nil {
		return Record{}, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("session: reading %s: %w", p, err)
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("session: %s is not readable: %w", p, err)
	}
	return r, nil
}

func (s State) update(id string, change func(*Record)) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	r, err := s.Get(id)
	if err != nil {
		return err
	}
	change(&r)
	if r == (Record{}) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(p, raw, 0o600)
}

// Link binds the session to a Task.
func (s State) Link(id, task string) error {
	return s.update(id, func(r *Record) { r.Task = task })
}

// MarkHandedOff leaves the marker: the session was seeded into task with the seed
// whose digest is given, and binds it to the Task. tree is that of the snapshot that
// went with it, "" if none did.
func (s State) MarkHandedOff(id, task, digest, tree string, at time.Time) error {
	return s.update(id, func(r *Record) {
		r.Task, r.Tree = task, tree
		r.HandedOff = &HandedOff{Task: task, Digest: digest, At: at.UTC()}
	})
}

// ClearHandedOff takes the marker away, as the pull of the Task does once the session is
// back. The binding to the Task stays.
func (s State) ClearHandedOff(id string) error {
	return s.update(id, func(r *Record) { r.HandedOff = nil })
}
