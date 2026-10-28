package redact_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

func TestAnEscapedSetRedactsTheTOMLAndJSONFormsOfAValue(t *testing.T) {
	const value = "pass\"word\\back/x\nta\tb-end\x01\x7f-1234"
	set, err := redact.NewEscapedSet([]redact.Needle{{Ref: "a/b", Value: []byte(value)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{
		value,
		`pass\"word\\back/x\nta\tb-end\u0001\u007f-1234`,
		`pass\"word\\back\/x\nta\tb-end\u0001\u007f-1234`,
		`pass\"word\\back/x\u000Ata\u0009b-end\u0001\u007F-1234`,
	} {
		if got := redact.Bytes([]byte("x "+form+" y"), set); string(got) != "x [REDACTED:a/b] y" {
			t.Errorf("%q is redacted as %q", form, got)
		}
	}
}

func TestAValueWithoutSpecialCharactersAddsNoNeedle(t *testing.T) {
	plain := []redact.Needle{{Ref: "a/b", Value: []byte("plain-secret-1234")}}
	escaped, err := redact.NewEscapedSet(plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := redact.NewSet(plain)
	in := []byte("a plain-secret-1234 b plain\\secret")
	if !bytes.Equal(redact.Bytes(in, escaped), redact.Bytes(in, raw)) {
		t.Error("the escaped set differs from the plain one for a plain value")
	}
}

func TestAnEscapedFormThatOverflowsTheBudgetFailsClosed(t *testing.T) {
	big := bytes.Repeat([]byte(`"`), redact.MaxNeedleBytes/2)
	if _, err := redact.NewSet([]redact.Needle{{Ref: "a/b", Value: big}}); err != nil {
		t.Fatalf("the raw value must fit: %v", err)
	}
	if _, err := redact.NewEscapedSet([]redact.Needle{{Ref: "a/b", Value: big}}); !errors.Is(err, redact.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}
