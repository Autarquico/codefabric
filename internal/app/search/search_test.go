package search_test

import (
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/app/search"
)

func TestMatchExpression_OnlyWordsSurvive(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"drain", `"drain"*`},
		{"worker drain", `"worker"* "drain"*`},
		{`"`, ``},
		{`*`, ``},
		{`-`, ``},
		{`:`, ``},
		{`"drain" OR "x"`, `"drain"* "OR"* "x"*`},
		{`NEAR(drain pool, 3)`, `"NEAR"* "drain"* "pool"* "3"*`},
		{`-drain`, `"drain"*`},
		{`name:drain`, `"name"* "drain"*`},
		{`dr*`, `"dr"*`},
		{`^a`, `"a"*`},
		{`d"rain`, `"d"* "rain"*`},
		{`café ñandú`, `"café"* "ñandú"*`},
		{`task-12`, `"task"* "12"*`},
		{"", ``},
	} {
		if got := search.MatchExpression(c.in); got != c.want {
			t.Errorf("MatchExpression(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchExpression_BoundsTheNumberOfTerms(t *testing.T) {
	got := search.MatchExpression(strings.Repeat("a ", 100))
	if n := strings.Count(got, `"a"*`); n != 12 {
		t.Errorf("%d terms, want 12", n)
	}
}
