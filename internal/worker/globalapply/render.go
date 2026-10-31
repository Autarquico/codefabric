package globalapply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// SecretResolver gives the value of a secret ref from the secret store of
// this Worker. An error must not carry the value.
type SecretResolver func(ref string) ([]byte, error)

// renderComponent is c with the markers of its files replaced by their values,
// each file with the escape the adapter names for it. It writes nothing; a
// missing secret, an adapter with no escape for a file or no resolver is an
// error wrapping harness.ErrSecretUnavailable. A rendered file is readable by
// its owner only, whatever mode the Workflow recorded for it.
func renderComponent(a harness.Adapter, resolve SecretResolver, c harness.Component) (harness.Component, error) {
	out := c
	out.Files = make([]harness.File, len(c.Files))
	for i, f := range c.Files {
		out.Files[i] = f
		if len(f.SecretRefs) == 0 {
			continue
		}
		if resolve == nil {
			return harness.Component{}, fmt.Errorf("%w: %s has the secret refs %q and this apply has no secret store", harness.ErrSecretUnavailable, f.Path, f.SecretRefs)
		}
		escaper, ok := a.(harness.SecretEscaper)
		if !ok {
			return harness.Component{}, fmt.Errorf("%w: harness %s says no escape for the secrets of %q", harness.ErrSecretUnavailable, a.ID(), f.Path)
		}
		esc, err := escaper.SecretEscape(f.Path)
		if err != nil {
			return harness.Component{}, fmt.Errorf("%w: %q: %w", harness.ErrSecretUnavailable, f.Path, err)
		}
		content, err := harness.RenderSecrets(f.Content, func(ref string) ([]byte, error) {
			v, err := resolve(ref)
			if err != nil {
				return nil, fmt.Errorf("%w: secret %q of %s: %w", harness.ErrSecretUnavailable, ref, f.Path, err)
			}
			return escapeValue(esc, v)
		})
		if err != nil {
			return harness.Component{}, err
		}
		out.Files[i].Content = content
		out.Files[i].SecretRefs = nil
		out.Files[i].Mode = f.Mode &^ 0o077 // a value is for this user alone
	}
	return out, nil
}

func escapeValue(e harness.Escape, v []byte) ([]byte, error) {
	switch e {
	case harness.EscapeRaw:
		return v, nil
	case harness.EscapeTOMLBasicString:
		return redact.TOMLBasicString(v)
	case harness.EscapeJSONString:
		if !utf8.Valid(v) {
			return nil, errors.New("the value is not valid UTF-8, and a JSON string is")
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(string(v)); err != nil {
			return nil, err
		}
		q := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		return q[1 : len(q)-1], nil
	default:
		return nil, fmt.Errorf("unknown escape %q", e)
	}
}
