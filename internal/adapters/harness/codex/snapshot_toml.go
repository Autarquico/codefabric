package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
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
func renderTable(t table) ([]byte, error) {
	b, err := toml.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("rendering TOML: %w", err)
	}
	return b, nil
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
