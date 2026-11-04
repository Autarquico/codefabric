package cli

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestAnswerOf_TakesExactlyOneOfAllowDenyText(t *testing.T) {
	for _, bad := range []struct {
		allow, deny bool
		text        string
	}{{}, {allow: true, deny: true}, {allow: true, text: "x"}, {deny: true, text: "x"}} {
		if _, err := answerOf(bad.allow, bad.deny, bad.text); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	for _, c := range []struct {
		allow, deny   bool
		text, decided string
	}{{true, false, "", domain.AnswerAllow}, {false, true, "", domain.AnswerDeny}, {false, false, "go on", ""}} {
		a, err := answerOf(c.allow, c.deny, c.text)
		if err != nil || a.Decision != c.decided || a.Text != c.text {
			t.Errorf("answerOf(%+v) = %+v, %v", c, a, err)
		}
	}
}
