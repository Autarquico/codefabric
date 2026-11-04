package secretstest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// RequireValueOutOfCatalog fails t unless the keyring entry ref of the catalog
// in raw (secrets.json) has no value field and value appears nowhere in it,
// neither as it is nor base64-encoded, as the file backend stores its values.
func RequireValueOutOfCatalog(t testing.TB, raw []byte, ref string, value []byte) {
	t.Helper()
	var c struct {
		Secrets map[string]map[string]json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("the catalog is not JSON: %v", err)
	}
	entry, ok := c.Secrets[ref]
	if !ok {
		t.Fatalf("the catalog has no entry %q", ref)
	}
	if _, has := entry["value"]; has {
		t.Errorf("the entry %q has a value field", ref)
	}
	encodings := []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding}
	forms := [][]byte{value}
	for _, e := range encodings {
		forms = append(forms, []byte(e.EncodeToString(value)))
	}
	for _, f := range forms {
		if bytes.Contains(raw, f) {
			t.Errorf("the catalog holds the value as %q", f)
		}
	}
}
