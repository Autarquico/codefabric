package redact

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// needlesOf lists the distinct needles of a Set and the ref that names each.
func needlesOf(s *Set) map[string]string {
	got := map[string]string{}
	var walk func(node int32, prefix []byte)
	walk = func(node int32, prefix []byte) {
		if n := s.term[node]; n >= 0 {
			marker := string(s.markers[s.refOf[n]])
			got[string(prefix)] = strings.TrimSuffix(strings.TrimPrefix(marker, "[REDACTED:"), "]")
		}
		for c := s.firstChild[node]; c >= 0; c = s.nextSibling[c] {
			walk(c, append(slices.Clip(prefix), s.label[c]))
		}
	}
	walk(0, nil)
	return got
}

func TestNewSetDerivesTheNeedlesAPTYCanPrint(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  []string
	}{
		"single line": {"s3cr3t-value", []string{"s3cr3t-value"}},
		"final newline": {"s3cr3t-value\n", []string{
			"s3cr3t-value\n", "s3cr3t-value", "s3cr3t-value\r\n",
		}},
		"final CRLF": {"s3cr3t-value\r\n", []string{
			"s3cr3t-value\r\n", "s3cr3t-value", "s3cr3t-value\r\r\n",
		}},
		"multi-line": {"first-line\nsecond-line\n", []string{
			"first-line\nsecond-line\n", "first-line\nsecond-line",
			"first-line\r\nsecond-line\r\n", "first-line\r\nsecond-line",
			"first-line", "second-line",
		}},
		"blank lines are no needles": {"first-line\n\nsecond-line", []string{
			"first-line\n\nsecond-line", "first-line\r\n\r\nsecond-line",
			"first-line", "second-line",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			set, err := NewSet([]Needle{{Ref: "r", Value: []byte(tc.value)}})
			if err != nil {
				t.Fatal(err)
			}
			got := needlesOf(set)
			want := map[string]string{}
			for _, n := range tc.want {
				want[n] = "r"
			}
			if !maps.Equal(got, want) {
				t.Errorf("needles = %q, want %q", got, want)
			}
		})
	}
}

func TestNewSetNamesEqualNeedlesAfterTheEarlierSecret(t *testing.T) {
	set, err := NewSet([]Needle{
		{Ref: "first", Value: []byte("same-value\n")},
		{Ref: "second", Value: []byte("same-value")},
		{Ref: "third", Value: []byte("same-value")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := needlesOf(set)["same-value"]; got != "first" {
		t.Errorf("same-value is named %q, want first", got)
	}
}

func TestNewSetRejectsWhatItCannotRedact(t *testing.T) {
	value := []byte("the-secret-value")
	for name, tc := range map[string]struct {
		needles []Needle
		want    error
	}{
		"short value":          {[]Needle{{Ref: "r", Value: []byte("1234567")}}, ErrTooShort},
		"short once trimmed":   {[]Needle{{Ref: "r", Value: []byte("1234567\r\n")}}, ErrTooShort},
		"empty value":          {[]Needle{{Ref: "r"}}, ErrTooShort},
		"short line":           {[]Needle{{Ref: "r", Value: []byte("long-enough\nshort\n")}}, ErrTooShort},
		"short line with CR":   {[]Needle{{Ref: "r", Value: []byte("long-enough\r\nshort\r\nlong-enough")}}, ErrTooShort},
		"empty ref":            {[]Needle{{Value: value}}, ErrInvalidRef},
		"ref breaks marker":    {[]Needle{{Ref: "a]b", Value: value}}, ErrInvalidRef},
		"ref with white space": {[]Needle{{Ref: "a b", Value: value}}, ErrInvalidRef},
		"ref too long":         {[]Needle{{Ref: strings.Repeat("r", maxRefLen+1), Value: value}}, ErrInvalidRef},
		"over budget": {[]Needle{
			{Ref: "a", Value: bytes.Repeat([]byte("a"), MaxNeedleBytes/2)},
			{Ref: "b", Value: bytes.Repeat([]byte("b"), MaxNeedleBytes/2+1)},
		}, ErrTooLarge},
		// The raw value fits; its derived needles do not.
		"derived over budget": {[]Needle{
			{Ref: "a", Value: bytes.Repeat([]byte("0123456789\n"), MaxNeedleBytes/11/3)},
		}, ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewSet(tc.needles)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, n := range tc.needles {
				if len(n.Value) > 0 && strings.Contains(err.Error(), string(n.Value)) {
					t.Errorf("the error repeats the value: %v", err)
				}
			}
		})
	}
}

func TestNewSetAcceptsTheWholeBudget(t *testing.T) {
	_, err := NewSet([]Needle{{Ref: "a", Value: bytes.Repeat([]byte("a"), MaxNeedleBytes)}})
	if err != nil {
		t.Fatal(err)
	}
}
