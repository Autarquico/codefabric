package redact

import "sync"

// Live is the secrets a process holds at the moment, by owner (a Run, say),
// for text that may name any of them: a log line is redacted of every value
// held, whoever it belongs to. It is safe for concurrent use; a nil *Live
// holds nothing and redacts nothing.
type Live struct {
	mu   sync.RWMutex
	sets map[string]*Set
}

// Hold makes the secrets of owner these, in place of those it held, matched in
// every form NewEscapedSet derives. On an error owner keeps what it held.
func (l *Live) Hold(owner string, secrets []Needle) error {
	if l == nil {
		return nil
	}
	if len(secrets) == 0 {
		l.Drop(owner)
		return nil
	}
	set, err := NewEscapedSet(secrets)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sets == nil {
		l.sets = map[string]*Set{}
	}
	l.sets[owner] = set
	return nil
}

// Drop forgets the secrets of owner.
func (l *Live) Drop(owner string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sets, owner)
}

// Text is s with every secret held redacted.
func (l *Live) Text(s string) string {
	if l == nil {
		return s
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, set := range l.sets {
		s = string(Bytes([]byte(s), set))
	}
	return s
}
