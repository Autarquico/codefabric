package redact_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"runtime"
	"strings"
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

func TestAnOversizedValueFailsClosedBeforeDerivingItsForms(t *testing.T) {
	huge := bytes.Repeat([]byte("\x01"), redact.MaxNeedleBytes+1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := redact.NewEscapedSet([]redact.Needle{{Ref: "a/b", Value: huge}})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, redact.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	// Deriving the forms of this value allocates several times its size.
	if got := after.TotalAlloc - before.TotalAlloc; got > uint64(len(huge)) {
		t.Errorf("allocated %d bytes for a value of %d: its forms were derived", got, len(huge))
	}
}

// redactSplit runs in through the streaming writer cut at every position.
func redactSplit(t *testing.T, set *redact.Set, in string) {
	t.Helper()
	for cut := 0; cut <= len(in); cut++ {
		var out bytes.Buffer
		w := redact.NewWriter(&out, set)
		for _, part := range []string{in[:cut], in[cut:]} {
			if _, err := w.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "x [REDACTED:git.origin] y" {
			t.Fatalf("cut at %d: %q", cut, got)
		}
	}
}

func TestAnEscapedSetRedactsThePercentEncodedValue(t *testing.T) {
	const value = "tok/en:with @sp ace+~-1"
	set, err := redact.NewEscapedSet([]redact.Needle{{Ref: "git.origin", Value: []byte(value)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{
		"tok%2Fen%3Awith%20%40sp%20ace%2B~-1",
		"tok%2fen%3awith%20%40sp%20ace%2b~-1",
		"tok%2Fen%3Awith+%40sp+ace%2B~-1",
	} {
		redactSplit(t, set, "x "+form+" y")
		if strings.Contains(string(redact.Bytes([]byte(form), set)), "ace") {
			t.Errorf("%q leaks", form)
		}
	}
}

func TestAnEscapedSetRedactsBase64OfUserAndToken(t *testing.T) {
	needle := redact.Needle{Ref: "git.origin", Value: []byte("ghp_tok?>en~~value"), User: "x-access-token"}
	set, err := redact.NewEscapedSet([]redact.Needle{needle})
	if err != nil {
		t.Fatal(err)
	}
	basic := []byte(needle.User + ":" + string(needle.Value))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		form := enc.EncodeToString(basic)
		redactSplit(t, set, "x "+form+" y")
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString(basic) + "\r\n"
	if got := string(redact.Bytes([]byte(header), set)); got != "Authorization: Basic [REDACTED:git.origin]=\r\n" && got != "Authorization: Basic [REDACTED:git.origin]\r\n" {
		t.Errorf("header redacted as %q", got)
	}
}

func TestWithoutAUserThereIsNoBase64Needle(t *testing.T) {
	value := []byte("plain-secret-1234")
	set, err := redact.NewEscapedSet([]redact.Needle{{Ref: "a/b", Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	form := base64.RawStdEncoding.EncodeToString(append([]byte("x-access-token:"), value...))
	if got := string(redact.Bytes([]byte(form), set)); got != form {
		t.Errorf("a base64 form was redacted without a User: %q", got)
	}
}

func TestThePercentAndBase64FormsStayInsideTheBudget(t *testing.T) {
	big := bytes.Repeat([]byte("/"), redact.MaxNeedleBytes/3)
	if _, err := redact.NewEscapedSet([]redact.Needle{{Ref: "a/b", Value: big, User: "u"}}); !errors.Is(err, redact.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}
