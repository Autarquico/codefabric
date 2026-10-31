package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// object is a parsed JSON object. Every operation on settings.json and
// .claude.json works on it, never on the text: quoting, escapes and key order
// are the encoder's. Numbers stay json.Number, so none loses digits.
type object = map[string]any

// parseObject is the JSON object of b, and nothing after it.
func parseObject(b []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o object
	if err := dec.Decode(&o); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if o == nil {
		return nil, errors.New("not a JSON object: null")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("not a JSON object: there is more after it")
	}
	return o, nil
}

// encodeObject is the canonical JSON of o: keys sorted, two spaces of indent,
// no HTML escapes, a final newline. The same object gives the same bytes.
func encodeObject(o object) ([]byte, error) { return encodeValue(o) }

// secretKeys are the keys whose object has a secret in each of its string
// values, wherever they are: `env` (settings and MCP servers) and `headers`
// (MCP servers and HTTP hooks). It is a closed policy: every value, not the
// ones that look like secrets. Everything else is copied as it is: the
// commands of apiKeyHelper, otelHeadersHelper and headersHelper, the args of
// an MCP server and every URL (README, "Known limit").
var secretKeys = []string{"env", "headers"}

// secretifySettings is settings.json canonical and without secret values.
func secretifySettings(raw []byte) ([]byte, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	if err := secretify(o, nil, "settings"); err != nil {
		return nil, err
	}
	return encodeObject(o)
}

// secretifyMCP is the mcpServers of the ~/.claude.json raw, canonical and
// without secret values, as a file of its own; false when it has none.
func secretifyMCP(raw []byte) ([]byte, bool, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, false, err
	}
	servers, ok := o[MCPComponent].(object)
	if !ok {
		return nil, false, nil
	}
	only := object{MCPComponent: servers}
	if err := secretify(only, nil, "claude.json"); err != nil {
		return nil, false, err
	}
	b, err := encodeObject(only)
	return b, err == nil, err
}

// secretify replaces, in place, every string value of an `env` or `headers`
// object of v with the marker of its ref. An empty value stays: it hides
// nothing. A value that already is a marker stays: it is a ref the person
// wrote.
func secretify(v any, path []string, file string) error {
	switch v := v.(type) {
	case object:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			here := append(slices.Clone(path), key)
			if obj, ok := v[key].(object); ok && slices.Contains(secretKeys, key) {
				for _, name := range slices.Sorted(maps.Keys(obj)) {
					if err := mark(obj, name, append(slices.Clone(here), name), file); err != nil {
						return err
					}
				}
				continue
			}
			if err := secretify(v[key], here, file); err != nil {
				return err
			}
		}
	case []any:
		for i, el := range v {
			if err := secretify(el, append(slices.Clone(path), strconv.Itoa(i)), file); err != nil {
				return err
			}
		}
	}
	return nil
}

func mark(o object, key string, path []string, file string) error {
	s, ok := o[key].(string)
	if !ok || s == "" || len(harness.SecretRefs([]byte(s))) > 0 {
		return nil
	}
	ref, err := secretRef(file, path)
	if err != nil {
		return err
	}
	o[key] = harness.SecretMarker(ref)
	return nil
}

// secretRef is the ref of the secret at path of file ("settings" or
// "claude.json"): claude/settings/env/<VAR>, or
// claude/claude.json/mcpServers/<server>/env/<VAR>. It depends only on the
// path; the file is in it so that the two JSON files cannot collide. A segment
// that has characters a ref cannot carry is sanitised and told apart by a hash
// of the original.
func secretRef(file string, path []string) (string, error) {
	segments := []string{refPrefix, file}
	for _, p := range path {
		segments = append(segments, refSegment(p))
	}
	ref := strings.Join(segments, "/")
	if !domain.ValidSecretRef(ref) {
		return "", fmt.Errorf("the secret at %s has no valid ref (%q)", strings.Join(path, "."), ref)
	}
	return ref, nil
}

func refSegment(s string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
	if clean == s && s != "" && s != "." && s != ".." {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// markerOpen is how every marker begins.
const markerOpen = "{{cf-secret:"

// checkJSONFile is an error when content is not a JSON object (with an
// mcpServers object, when mcp), or when a secret marker is anywhere but inside
// a string value: in a key, which would be rendered as a value. A marker in a
// key is refused whatever its escapes.
func checkJSONFile(content []byte, mcp bool) error {
	o, err := parseObject(content)
	if err != nil {
		return err
	}
	if mcp {
		if _, ok := o[MCPComponent].(object); !ok || len(o) != 1 {
			return fmt.Errorf("wants one field, %s, that is an object", MCPComponent)
		}
	}
	inValues, err := markersInValues(o)
	if err != nil {
		return err
	}
	if bytes.Count(content, []byte(markerOpen)) != inValues {
		return fmt.Errorf("%w: a secret marker is not inside a JSON string value", harness.ErrSecretUnavailable)
	}
	return nil
}

func markersInValues(v any) (int, error) {
	n := 0
	switch v := v.(type) {
	case object:
		for k, el := range v {
			if strings.Contains(k, markerOpen) {
				return 0, fmt.Errorf("%w: a key of the configuration has a secret marker", harness.ErrSecretUnavailable)
			}
			c, err := markersInValues(el)
			if err != nil {
				return 0, err
			}
			n += c
		}
	case []any:
		for _, el := range v {
			c, err := markersInValues(el)
			if err != nil {
				return 0, err
			}
			n += c
		}
	case string:
		n = strings.Count(v, markerOpen)
	}
	return n, nil
}

// refPrefix starts every ref the adapter makes; it is the one of
// OAuthTokenRef too.
const refPrefix = "claude"
