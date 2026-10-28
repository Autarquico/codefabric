package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// table is a parsed TOML table. Every operation on config.toml works on it,
// never on the text: quoting, escapes and key order are the parser's.
type table = map[string]any

func parseTable(b []byte) (table, error) {
	t := table{}
	if err := toml.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("not valid TOML: %w", err)
	}
	return t, nil
}

// renderTable is the TOML of t. The encoder sorts the keys, so the same
// table gives the same bytes.
//
// go-toml writes a string as a literal ('...') when it can, and the secrets
// of a Run are written inside basic strings ("..."), the escape the host
// renders them with. So each string that has a marker is written by this
// function as a basic string, and the result is checked against t.
func renderTable(t table) ([]byte, error) {
	shield := &markerShield{prefix: freePrefix(t)}
	shielded, err := shield.table(t)
	if err != nil {
		return nil, err
	}
	b, err := toml.Marshal(shielded)
	if err != nil {
		return nil, fmt.Errorf("rendering TOML: %w", err)
	}
	if len(shield.originals) == 0 {
		return b, nil
	}
	if b, err = shield.restore(b); err != nil {
		return nil, err
	}
	back, err := parseTable(b)
	if err != nil || !reflect.DeepEqual(back, t) {
		return nil, errors.New("rendering TOML: the result does not parse back to the table")
	}
	if err := checkMarkersQuoted(b); err != nil {
		return nil, err
	}
	return b, nil
}

// markerShield stands in for the strings with markers while the encoder runs:
// a placeholder has a single quote, which makes the encoder write it as a
// basic string, and is replaced afterwards, quotes included, by the string
// written as a basic string.
type markerShield struct {
	prefix    string
	originals []string
}

// freePrefix is a placeholder prefix that no key or string of t contains, so
// that a value the person wrote is never taken for a placeholder.
func freePrefix(t table) string {
	for n := 0; ; n++ {
		prefix := "'cf-marker-" + strconv.Itoa(n) + "-"
		if !mentions(t, prefix) {
			return prefix
		}
	}
}

func mentions(v any, text string) bool {
	switch v := v.(type) {
	case table:
		for k, el := range v {
			if strings.Contains(k, text) || mentions(el, text) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, func(el any) bool { return mentions(el, text) })
	case string:
		return strings.Contains(v, text)
	}
	return false
}

func (m *markerShield) table(t table) (table, error) {
	out := make(table, len(t))
	for k, v := range t {
		if strings.Contains(k, markerOpen) {
			return nil, fmt.Errorf("%w: a key of the configuration has a secret marker", harness.ErrSecretUnavailable)
		}
		sv, err := m.value(v)
		if err != nil {
			return nil, err
		}
		out[k] = sv
	}
	return out, nil
}

func (m *markerShield) value(v any) (any, error) {
	switch v := v.(type) {
	case table:
		return m.table(v)
	case []any:
		out := make([]any, len(v))
		for i, el := range v {
			var err error
			if out[i], err = m.value(el); err != nil {
				return nil, err
			}
		}
		return out, nil
	case string:
		if strings.Contains(v, markerOpen) {
			m.originals = append(m.originals, v)
			return fmt.Sprintf("%s%d'", m.prefix, len(m.originals)-1), nil
		}
	}
	return v, nil
}

func (m *markerShield) restore(b []byte) ([]byte, error) {
	if bytes.Count(b, []byte(`"`+m.prefix)) != len(m.originals) {
		return nil, errors.New("rendering TOML: a placeholder is not where it was put")
	}
	for i, s := range m.originals {
		inner, err := redact.TOMLBasicString([]byte(s))
		if err != nil {
			return nil, fmt.Errorf("rendering TOML: %w", err)
		}
		holder := []byte(fmt.Sprintf(`"%s%d'"`, m.prefix, i))
		if bytes.Count(b, holder) != 1 {
			return nil, errors.New("rendering TOML: a placeholder is not where it was put")
		}
		b = bytes.Replace(b, holder, append(append([]byte(`"`), inner...), '"'), 1)
	}
	return b, nil
}

// markerOpen is how every marker begins.
const markerOpen = "{{cf-secret:"

// checkMarkersQuoted is an error, wrapping harness.ErrSecretUnavailable, when
// content has a marker anywhere but inside a TOML basic string ("..."): in a
// literal string, a key, a comment. A marker there would be rendered with the
// escape of a basic string and written wrong, or break the file.
func checkMarkersQuoted(content []byte) error {
	var p unstable.Parser
	p.Reset(content)
	quoted := 0
	for p.NextExpression() {
		if node := p.Expression(); node.Kind == unstable.KeyValue {
			if err := quotedMarkers(&p, node.Value(), &quoted); err != nil {
				return err
			}
		}
	}
	if err := p.Error(); err != nil {
		return fmt.Errorf("not valid TOML: %w", err)
	}
	if bytes.Count(content, []byte(markerOpen)) != quoted {
		return fmt.Errorf("%w: a secret marker is not inside a TOML basic string", harness.ErrSecretUnavailable)
	}
	return nil
}

// quotedMarkers adds to quoted the markers inside the basic strings of n, and
// is an error for a string with markers that is not a basic one.
func quotedMarkers(p *unstable.Parser, n *unstable.Node, quoted *int) error {
	switch n.Kind {
	case unstable.String:
		raw := p.Raw(n.Raw)
		count := bytes.Count(raw, []byte(markerOpen))
		if count == 0 {
			return nil
		}
		if len(raw) < 2 || raw[0] != '"' || bytes.HasPrefix(raw, []byte(`"""`)) {
			return fmt.Errorf("%w: a secret marker is not inside a TOML basic string", harness.ErrSecretUnavailable)
		}
		*quoted += count
	case unstable.Array, unstable.InlineTable:
		for it := n.Children(); it.Next(); {
			child := it.Node()
			if child.Kind == unstable.KeyValue {
				child = child.Value()
			}
			if err := quotedMarkers(p, child, quoted); err != nil {
				return err
			}
		}
	}
	return nil
}

// localKeys are the top-level keys that describe the machine, not the
// configuration: `projects` is the trust of each path of this machine.
var localKeys = []string{"projects"}

// secretTables are the keys whose table has a secret in each of its string
// values (config.schema.json of codex 0.160.1): the environment and headers
// of an MCP server, the headers and query of a model provider.
var secretTables = []string{"env", "http_headers", "query_params"}

// secretScalars are the keys whose string value is a secret.
var secretScalars = []string{"experimental_bearer_token", "bearer_token"}

// The keys that name an environment variable (env_vars, env_http_headers,
// env_key, bearer_token_env_var) are in none of the lists: a name is not a
// secret, and Codex needs it to find the value.

// otelExporterKinds are the tables of an OTEL exporter that send headers
// (OtelExporterKind, under otel.exporter, otel.trace_exporter and
// otel.metrics_exporter).
var otelExporterKinds = []string{"otlp-http", "otlp-grpc"}

// holdsSecrets says whether the table at path has secrets as values: every
// string, or every string of the lists when the values are lists. The match
// headers of a MITM hook (permissions.<p>.network.mitm.hooks.<h>.headers) are
// the lists.
func holdsSecrets(path []string) (secrets, lists bool) {
	n := len(path)
	key, parent := path[n-1], last(path[:n-1])
	switch {
	case slices.Contains(secretTables, key),
		key == "set" && parent == "shell_environment_policy",
		key == "headers" && slices.Contains(otelExporterKinds, parent):
		return true, false
	case key == "headers" && n >= 4 && path[n-3] == "hooks" && path[n-4] == "mitm":
		return true, true
	}
	return false, false
}

// secretifyTable drops what is local to the machine and replaces every
// secret value of t with its marker, in place.
func secretifyTable(t table) error {
	for _, k := range localKeys {
		delete(t, k)
	}
	return secretify(t, nil)
}

func secretify(t table, path []string) error {
	for _, key := range slices.Sorted(mapKeys(t)) {
		here := append(slices.Clone(path), key)
		switch v := t[key].(type) {
		case table:
			secrets, lists := holdsSecrets(here)
			if !secrets {
				if err := secretify(v, here); err != nil {
					return err
				}
				continue
			}
			for _, name := range slices.Sorted(mapKeys(v)) {
				mark := markString
				if lists {
					mark = markStrings
				}
				if err := mark(v, name, append(slices.Clone(here), name)); err != nil {
					return err
				}
			}
		case []any:
			for i, el := range v {
				if sub, ok := el.(table); ok {
					if err := secretify(sub, append(slices.Clone(here), strconv.Itoa(i))); err != nil {
						return err
					}
				}
			}
		case string:
			if slices.Contains(secretScalars, key) {
				if err := markString(t, key, here); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// markString replaces t[key], when it is a string, with the marker of the
// ref of path.
func markString(t table, key string, path []string) error {
	v, err := marked(t[key], path)
	t[key] = v
	return err
}

// markStrings replaces each string of the list t[key] with its marker; the
// index of the string ends its ref.
func markStrings(t table, key string, path []string) error {
	list, ok := t[key].([]any)
	if !ok {
		return nil
	}
	for i := range list {
		v, err := marked(list[i], append(slices.Clone(path), strconv.Itoa(i)))
		if err != nil {
			return err
		}
		list[i] = v
	}
	return nil
}

// marked is v as a secret: the marker of the ref of path when v is a string.
// A value that already is a marker stays: it is a ref the person wrote.
func marked(v any, path []string) (any, error) {
	s, ok := v.(string)
	if !ok || len(harness.SecretRefs([]byte(s))) > 0 {
		return v, nil
	}
	ref, err := secretRef(path)
	if err != nil {
		return v, err
	}
	return harness.SecretMarker(ref), nil
}

// secretRef is the ref of the secret at path: codex/mcp/<server>/env/<VAR>
// for mcp_servers.<server>.env.<VAR>. It depends only on the path. A segment
// that has characters a ref cannot carry is sanitised and told apart by a
// hash of the original.
func secretRef(path []string) (string, error) {
	segments := make([]string, 0, len(path)+1)
	segments = append(segments, ID)
	for i, p := range path {
		if i == 0 && p == "mcp_servers" {
			p = "mcp"
		}
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

func last(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// leafPaths are the paths of the values of t that are not tables, and of the
// empty tables, in key order: the settings a snapshot carries.
func leafPaths(t table) [][]string {
	var out [][]string
	var walk func(t table, path []string)
	walk = func(t table, path []string) {
		for _, k := range slices.Sorted(mapKeys(t)) {
			here := append(slices.Clone(path), k)
			if sub, ok := t[k].(table); ok && len(sub) > 0 {
				walk(sub, here)
				continue
			}
			out = append(out, here)
		}
	}
	walk(t, nil)
	return out
}

func lookup(t table, path []string) (any, bool) {
	var cur any = t
	for _, k := range path {
		m, ok := cur.(table)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// assign sets the value at path in t, making the tables on the way.
func assign(t table, path []string, v any) {
	for _, k := range path[:len(path)-1] {
		next, ok := t[k].(table)
		if !ok {
			next = table{}
			t[k] = next
		}
		t = next
	}
	t[path[len(path)-1]] = v
}

// unassign deletes the value at path from t, and the tables it leaves empty.
func unassign(t table, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(t, path[0])
		return
	}
	sub, ok := t[path[0]].(table)
	if !ok {
		return
	}
	unassign(sub, path[1:])
	if len(sub) == 0 {
		delete(t, path[0])
	}
}

// containsSubset says whether host has, at each leaf path of want, the value
// want has there.
func containsSubset(host, want table) bool {
	for _, p := range leafPaths(want) {
		w, _ := lookup(want, p)
		if h, ok := lookup(host, p); !ok || !reflect.DeepEqual(h, w) {
			return false
		}
	}
	return true
}
