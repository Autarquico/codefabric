package harness_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

func TestSetJSONStrings(t *testing.T) {
	cases := []struct {
		name, line string
		edits      map[string]string
		want       string
	}{
		{"top level", `{"a": 1, "cwd":"/x" ,"b":[1,{"cwd":"/y"}]}`, map[string]string{"cwd": "/new"}, `{"a": 1, "cwd":"/new" ,"b":[1,{"cwd":"/y"}]}`},
		{"nested path", `{"type":"m","payload":{"id":"1","cwd":"/x","deep":{"cwd":"/z"}},"cwd":"/top"}`, map[string]string{"payload.cwd": "/n"}, `{"type":"m","payload":{"id":"1","cwd":"/n","deep":{"cwd":"/z"}},"cwd":"/top"}`},
		{"several, in order", `{"id":"a","payload":{"id":"a","cwd":"/x"}}`, map[string]string{"id": "b", "payload.cwd": "/n"}, `{"id":"b","payload":{"id":"a","cwd":"/n"}}`},
		{"descends past an array", `{"payload":[{"cwd":"/q"}],"x":1}`, map[string]string{"payload.cwd": "/n"}, `{"payload":[{"cwd":"/q"}],"x":1}`},
		{"not a string", `{"cwd":7,"payload":{"cwd":[3]}}`, map[string]string{"cwd": "/n", "payload.cwd": "/n"}, `{"cwd":7,"payload":{"cwd":[3]}}`},
		{"escapes without HTML", `{"cwd":"/x"}`, map[string]string{"cwd": `a"<&>ü`}, `{"cwd":"a\"<&>ü"}`},
		{"absent", `{"a":1}`, map[string]string{"payload.cwd": "/n"}, `{"a":1}`},
	}
	for _, c := range cases {
		got, err := harness.SetJSONStrings([]byte(c.line), c.edits)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s, %v; want %s", c.name, got, err, c.want)
		}
	}
	for _, bad := range []string{`[1]`, `{"a":1} x`, `{"a":`, `garbage`, ``} {
		if _, err := harness.SetJSONStrings([]byte(bad), map[string]string{"a": "b"}); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestMapJSONLinesEndsEveryLineWithANewlineAndSaysWhichFailed(t *testing.T) {
	out, err := harness.MapJSONLines([]byte("{\"a\":1}\n{\"a\":2}"), func(l []byte) ([]byte, error) { return harness.SetJSONStrings(l, nil) })
	if err != nil || string(out) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Errorf("%q, %v", out, err)
	}
	if _, err := harness.MapJSONLines([]byte("{}\nnope\n"), func(l []byte) ([]byte, error) { return harness.SetJSONStrings(l, nil) }); err == nil {
		t.Error("a line that is not JSON passed")
	}
}
