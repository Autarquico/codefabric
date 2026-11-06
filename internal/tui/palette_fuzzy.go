package tui

import "strings"

// fuzzy ranks what the palette lists against what the person typed. Every word
// of the query has to match; a word matches a field where it appears whole
// (better at the start of the field or of one of its words) or, failing that,
// as scattered letters in order. The best field of each word counts, weighted by
// how much a person means by it: the title most.

const (
	weightTitle = 3
	weightOther = 2
)

// fuzzyScore is 0 when query does not match; higher is a better match. The
// fields are, in order of weight, the title and then the ones that follow.
func fuzzyScore(query string, title string, others ...string) int {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return 1
	}
	total := 0
	for _, w := range words {
		best := weightTitle * wordScore(w, strings.ToLower(title))
		for _, f := range others {
			best = max(best, weightOther*wordScore(w, strings.ToLower(f)))
		}
		if best == 0 {
			return 0
		}
		total += best
	}
	return total
}

func wordScore(word, field string) int {
	if i := strings.Index(field, word); i >= 0 {
		score := 100 - min(i, 40)
		if i == 0 || isSeparator(field[i-1]) {
			score += 40
		}
		return score
	}
	// Letters in order, as in "wkdr" for "worker drain". The tighter the better,
	// and always below a whole word.
	at, span := 0, 0
	first := -1
	for _, r := range word {
		j := strings.IndexRune(field[at:], r)
		if j < 0 {
			return 0
		}
		if first < 0 {
			first = at + j
		}
		at += j + len(string(r))
		span = at - first
	}
	return max(50-(span-len(word))*2, 1)
}

func isSeparator(b byte) bool { return b == ' ' || b == '.' || b == '-' || b == '_' || b == '/' }
