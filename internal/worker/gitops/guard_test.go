package gitops

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

const (
	pem = "-----BEGIN KEY-----\nAAAAAAAAbbbbbbbbCCCCCCCC\nddddddddEEEEEEEE\n-----END KEY-----\n"
	tok = "ghp-unit-sentinel-0123456789"
)

// batch is what `git cat-file --batch` prints for objects with these contents.
func batch(objects ...string) string {
	var b strings.Builder
	for i, o := range objects {
		fmt.Fprintf(&b, "%040x blob %d\n%s\n", i+1, len(o), o)
	}
	return b.String()
}

// scan runs stream through a scanner of values, in writes of size bytes.
func scan(t *testing.T, stream string, size int, values ...redact.Needle) *scanner {
	t.Helper()
	sc, err := newScanner(values)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	sc.stop = func() { stopped = true }
	for p := []byte(stream); len(p) > 0; {
		k := min(size, len(p))
		if _, err := sc.Write(p[:k]); err != nil {
			break
		}
		p = p[k:]
	}
	if (sc.hit != "" || sc.err != nil) != stopped {
		t.Errorf("hit %q err %v, stopped %v", sc.hit, sc.err, stopped)
	}
	return sc
}

var (
	token = redact.Needle{Ref: "github.personal", Value: []byte(tok)}
	key   = redact.Needle{Ref: "deploy.key", Value: []byte(pem)}
)

func TestScanner_FindsAValueHoweverTheStreamIsCut(t *testing.T) {
	for name, c := range map[string]struct {
		objects []string
		want    string
	}{
		"nothing":                  {[]string{"a", "", "plain text"}, ""},
		"the token alone":          {[]string{tok}, "github.personal"},
		"the token in the middle":  {[]string{"x", "before " + tok + " after"}, "github.personal"},
		"the token at the end":     {[]string{strings.Repeat("y", 70000) + tok}, "github.personal"},
		"the token split in two":   {[]string{tok[:10], tok[10:]}, ""},
		"one line of the key":      {[]string{"ddddddddEEEEEEEE"}, "deploy.key"},
		"the key through a PTY":    {[]string{strings.ReplaceAll(pem, "\n", "\r\n")}, "deploy.key"},
		"a prefix of the token":    {[]string{tok[:len(tok)-1]}, ""},
		"a marker written as text": {[]string{"[REDACTED:github.personal] [REDACTED:deploy.key]"}, ""},
		"after an empty object":    {[]string{"", tok}, "github.personal"},
	} {
		for _, size := range []int{1, 3, 7, 4096, 1 << 20} {
			sc := scan(t, batch(c.objects...), size, token, key)
			if sc.hit != c.want || sc.err != nil {
				t.Errorf("%s, writes of %d: hit %q err %v, want %q", name, size, sc.hit, sc.err, c.want)
			}
			if c.want == "" && !sc.done() {
				t.Errorf("%s, writes of %d: the stream was not read whole", name, size)
			}
		}
	}
}

func TestScanner_WhatItCannotReadIsAnError(t *testing.T) {
	for name, stream := range map[string]string{
		"a missing object": strings.Repeat("a", 40) + " missing\n",
		"a bad size":       strings.Repeat("a", 40) + " blob x\n",
		"no line break":    strings.Repeat("a", 40) + " blob 1\nzz",
		"a long header":    strings.Repeat("h", 600),
	} {
		sc := scan(t, stream, 5, token)
		if sc.err == nil && sc.done() {
			t.Errorf("%s: read as fine", name)
		}
	}
	if sc := scan(t, strings.Repeat("a", 40)+" blob 9\nabc", 5, token); sc.done() {
		t.Error("a truncated object is done")
	}
}

func TestScanner_RefusesAValueItCouldNotTellFromItsMarker(t *testing.T) {
	_, err := newScanner([]redact.Needle{{Ref: "odd", Value: []byte("x [REDACTED:odd] y")}})
	var b *BlockedError
	if !errors.As(err, &b) || strings.Contains(err.Error(), "[REDACTED:odd] y") {
		t.Errorf("err = %v", err)
	}
	_, err = newScanner([]redact.Needle{{Ref: "short", Value: []byte("tiny")}})
	if !errors.As(err, &b) || strings.Contains(err.Error(), "tiny") {
		t.Errorf("a value too short to look for: %v", err)
	}
}
