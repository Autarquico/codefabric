package redact

import (
	"strings"
	"testing"
)

func TestLive_RedactsWhatEveryOwnerHoldsUntilItIsDropped(t *testing.T) {
	var l Live
	if err := l.Hold("run-a", []Needle{{Ref: "a", Value: []byte("alpha-secret-value")}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Hold("run-b", []Needle{{Ref: "b", Value: []byte(`beta"secret`)}}); err != nil {
		t.Fatal(err)
	}
	got := l.Text(`a=alpha-secret-value b={"v":"beta\"secret"}`)
	if strings.Contains(got, "alpha-secret") || strings.Contains(got, "beta") || !strings.Contains(got, "[REDACTED:a]") || !strings.Contains(got, "[REDACTED:b]") {
		t.Fatalf("Text = %q", got)
	}
	if err := l.Hold("run-a", []Needle{{Ref: "a", Value: []byte("short")}}); err == nil {
		t.Fatal("a needle NewSet refuses was held")
	}
	if got := l.Text("alpha-secret-value"); strings.Contains(got, "alpha") {
		t.Fatalf("a refused Hold dropped what was held: %q", got)
	}
	l.Drop("run-a")
	if got := l.Text("alpha-secret-value"); got != "alpha-secret-value" {
		t.Fatalf("after Drop: %q", got)
	}
	var none *Live
	if none.Text("x") != "x" || none.Hold("o", nil) != nil {
		t.Fatal("a nil Live is not inert")
	}
}
