package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// ShortID is the human-friendly "<kind>-<n>" alias (§3.13).
type ShortID struct {
	Kind Kind
	N    uint64
}

func (s ShortID) String() string { return fmt.Sprintf("%s-%d", s.Kind, s.N) }

// ParseShortID accepts only the canonical rendering of a ShortID.
func ParseShortID(text string) (ShortID, error) {
	kindText, number, found := strings.Cut(text, "-")
	kind := Kind(kindText)
	n, err := strconv.ParseUint(number, 10, 64)
	short := ShortID{Kind: kind, N: n}
	if !found || !knownKinds[kind] || err != nil || n == 0 || short.String() != text {
		return ShortID{}, fmt.Errorf("%w: %q", ErrMalformedID, text)
	}
	return short, nil
}

// Candidate is what Resolve needs to know about an existing entity.
type Candidate struct {
	ID        ID
	Short     ShortID
	Name      string
	ProjectID ID
	Archived  bool
}

const minULIDPrefixLength = 6

// Resolve applies the §3.13 order: canonical ID, short ID, name, ULID prefix.
// cands must already be filtered by kind. Names only match unarchived
// candidates; scopeProject narrows a name only when it is ambiguous.
func Resolve(ref string, kind Kind, scopeProject ID, cands []Candidate) (ID, error) {
	if ref == "" {
		return "", notFound(ref, kind)
	}
	steps := []func() []Candidate{
		func() []Candidate { return matchCanonicalID(ref, kind, cands) },
		func() []Candidate { return matchShortID(ref, kind, cands) },
		func() []Candidate { return narrowToProject(matchName(ref, cands), scopeProject) },
		func() []Candidate { return matchULIDPrefix(ref, cands) },
	}
	for _, step := range steps {
		switch matches := step(); len(matches) {
		case 0:
			continue
		case 1:
			return matches[0].ID, nil
		default:
			return "", NewError(CodeAmbiguousRef,
				fmt.Sprintf("reference %q matches %d %ss", ref, len(matches), kind),
				map[string]any{"ref": ref, "candidates": idsOf(matches)})
		}
	}
	return "", notFound(ref, kind)
}

func notFound(ref string, kind Kind) *Error {
	return NewError(CodeNotFound,
		fmt.Sprintf("no %s matches reference %q", kind, ref),
		map[string]any{"ref": ref, "kind": string(kind)})
}

func idsOf(cands []Candidate) []ID {
	ids := make([]ID, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
	}
	return ids
}

func matchCanonicalID(ref string, kind Kind, cands []Candidate) []Candidate {
	if parsed, _, err := ParseID(ref); err != nil || parsed != kind {
		return nil
	}
	return filter(cands, func(c Candidate) bool { return string(c.ID) == ref })
}

func matchShortID(ref string, kind Kind, cands []Candidate) []Candidate {
	short, err := ParseShortID(ref)
	if err != nil || short.Kind != kind {
		return nil
	}
	return filter(cands, func(c Candidate) bool { return c.Short == short })
}

func matchName(ref string, cands []Candidate) []Candidate {
	return filter(cands, func(c Candidate) bool { return !c.Archived && c.Name == ref })
}

func narrowToProject(matches []Candidate, project ID) []Candidate {
	if len(matches) < 2 || project == "" {
		return matches
	}
	inProject := filter(matches, func(c Candidate) bool { return c.ProjectID == project })
	if len(inProject) == 0 {
		return matches
	}
	return inProject
}

func matchULIDPrefix(ref string, cands []Candidate) []Candidate {
	if len(ref) < minULIDPrefixLength {
		return nil
	}
	prefix := strings.ToUpper(ref)
	return filter(cands, func(c Candidate) bool { return strings.HasPrefix(c.ID.ulidPart(), prefix) })
}

func filter(cands []Candidate, keep func(Candidate) bool) []Candidate {
	var kept []Candidate
	for _, c := range cands {
		if keep(c) {
			kept = append(kept, c)
		}
	}
	return kept
}
