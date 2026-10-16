package redact

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	// MinSecretLen is the shortest value, and the shortest line of a multi-line
	// value, that a Set accepts: a shorter one would shred unrelated output
	// and could not be redacted reliably (Appendix B5).
	MinSecretLen = 8
	// MaxNeedleBytes bounds the bytes of every needle of a Set, derived ones
	// included, and with it the memory of the matcher.
	MaxNeedleBytes = 1 << 20
	maxRefLen      = 128
)

// Errors of NewSet. They name the ref, never the value.
var (
	ErrInvalidRef = errors.New("redact: invalid secret ref")
	ErrTooShort   = errors.New("redact: secret too short to redact")
	ErrTooLarge   = errors.New("redact: secrets exceed the redaction budget")
)

// Needle is a secret to redact: its value and the ref that names it in the
// marker that replaces it.
type Needle struct {
	Ref   string
	Value []byte
}

// Set is the immutable matcher of a group of secrets: an Aho-Corasick
// automaton over every needle derived from them. It is safe for concurrent use.
type Set struct {
	markers [][]byte // by ref index: "[REDACTED:<ref>]"
	lens    []int32  // by needle index
	refOf   []int32  // by needle index

	// The trie. Node 0 is the root; a node's children are a linked list.
	root        [256]int32 // child of the root by byte; 0 is none
	label       []byte
	firstChild  []int32
	nextSibling []int32
	fail        []int32
	dict        []int32 // nearest proper suffix node that ends a needle; -1 none
	term        []int32 // needle that ends at the node; -1 none
	// live is the depth of the longest suffix of the node that can still
	// grow into a longer needle; output older than that is final.
	live []int32
}

// NewSet derives the needles of the secrets and builds their matcher. Besides
// each value it matches the value without its final line break and, since a
// PTY turns "\n" into "\r\n", both of them with that transformation, plus each
// non-empty line of a multi-line value. When derived needles of two secrets are
// equal, the earlier secret names them.
//
// It never drops a needle: a value or a non-empty line shorter than
// MinSecretLen, an invalid ref, or needles beyond MaxNeedleBytes are an error.
func NewSet(secrets []Needle) (*Set, error) {
	s := &Set{}
	s.addNode(0)
	seen := map[string]bool{}
	total := 0
	for _, secret := range secrets {
		if !validRef(secret.Ref) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidRef, secret.Ref)
		}
		needles, err := derive(secret.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", err, secret.Ref)
		}
		ref := int32(len(s.markers)) //nolint:gosec // bounded by MaxNeedleBytes/MinSecretLen
		s.markers = append(s.markers, []byte("[REDACTED:"+secret.Ref+"]"))
		for _, n := range needles {
			if seen[string(n)] {
				continue
			}
			seen[string(n)] = true
			if total += len(n); total > MaxNeedleBytes {
				return nil, fmt.Errorf("%w: at %q", ErrTooLarge, secret.Ref)
			}
			s.insert(n, ref)
		}
	}
	s.link()
	return s, nil
}

// derive returns the needles of a value, possibly repeated.
func derive(value []byte) ([][]byte, error) {
	trimmed := bytes.TrimSuffix(value, []byte("\n"))
	if len(trimmed) < len(value) {
		trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
	}
	if len(trimmed) < MinSecretLen {
		return nil, ErrTooShort
	}
	needles := [][]byte{value, trimmed, onlcr(value), onlcr(trimmed)}
	if !bytes.Contains(trimmed, []byte("\n")) {
		return needles, nil
	}
	for line := range bytes.SplitSeq(trimmed, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch {
		case len(line) == 0:
		case len(line) < MinSecretLen:
			return nil, ErrTooShort
		default:
			needles = append(needles, line)
		}
	}
	return needles, nil
}

// onlcr is what a PTY prints for b.
func onlcr(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")) }

// validRef keeps the marker unambiguous: the bytes of the ref grammar of the
// secret ports, which this stdlib-only package cannot import.
func validRef(ref string) bool {
	if ref == "" || len(ref) > maxRefLen {
		return false
	}
	for _, c := range []byte(ref) {
		ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '.' || c == '_' || c == '/' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func (s *Set) addNode(c byte) int32 {
	s.label = append(s.label, c)
	s.firstChild = append(s.firstChild, -1)
	s.nextSibling = append(s.nextSibling, -1)
	s.fail = append(s.fail, 0)
	s.dict = append(s.dict, -1)
	s.term = append(s.term, -1)
	s.live = append(s.live, 0)
	return int32(len(s.label) - 1) //nolint:gosec // bounded by MaxNeedleBytes
}

func (s *Set) child(node int32, c byte) int32 {
	if node == 0 {
		if n := s.root[c]; n != 0 {
			return n
		}
		return -1
	}
	for n := s.firstChild[node]; n >= 0; n = s.nextSibling[n] {
		if s.label[n] == c {
			return n
		}
	}
	return -1
}

func (s *Set) insert(needle []byte, ref int32) {
	node := int32(0)
	for _, c := range needle {
		next := s.child(node, c)
		if next < 0 {
			next = s.addNode(c)
			s.nextSibling[next] = s.firstChild[node]
			s.firstChild[node] = next
			if node == 0 {
				s.root[c] = next
			}
		}
		node = next
	}
	s.term[node] = int32(len(s.lens))           //nolint:gosec // bounded by MaxNeedleBytes
	s.lens = append(s.lens, int32(len(needle))) //nolint:gosec // bounded by MaxNeedleBytes
	s.refOf = append(s.refOf, ref)
}

// link computes the failure, dictionary and live links breadth first, so that
// every shallower node is done before its use.
func (s *Set) link() {
	depth := make([]int32, len(s.label))
	queue := []int32{0}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if s.firstChild[node] >= 0 {
			s.live[node] = depth[node]
		} else {
			s.live[node] = s.live[s.fail[node]]
		}
		for n := s.firstChild[node]; n >= 0; n = s.nextSibling[n] {
			depth[n] = depth[node] + 1
			if node != 0 {
				s.fail[n] = s.step(s.fail[node], s.label[n])
			}
			if f := s.fail[n]; s.term[f] >= 0 {
				s.dict[n] = f
			} else {
				s.dict[n] = s.dict[f]
			}
			queue = append(queue, n)
		}
	}
}

// step is the automaton transition.
func (s *Set) step(node int32, c byte) int32 {
	for {
		if next := s.child(node, c); next >= 0 {
			return next
		}
		if node == 0 {
			return 0
		}
		node = s.fail[node]
	}
}
