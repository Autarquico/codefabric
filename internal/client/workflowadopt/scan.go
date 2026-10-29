// Package workflowadopt prepares a native configuration to be adopted into a
// Workflow: it finds the secrets a snapshot still carries, and replaces the
// ones the user confirms with secret markers, so that no value is ever
// uploaded (spec "Secretos en settings").
package workflowadopt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Errors of [Apply]. They name a component, a file and a line, never a value.
var (
	// ErrNotReplaceable is a secret that cannot be replaced by a marker.
	ErrNotReplaceable = errors.New("workflowadopt: secret cannot be replaced by a marker")
)

// Substituted is a ref the adapter already put in a file, in place of a value.
// The user confirms them as well.
type Substituted struct {
	Component string
	File      string
	Ref       string
}

// Hit is a value that looks like a secret and is still in the content.
type Hit struct {
	Component string
	File      string
	// Line is 1-based; for a PEM block, the line of its header.
	Line int
	// Kind is what the value looks like, e.g. "github-token".
	Kind string
	// Preview is redacted: the fixed prefix of the kind, if any, and the
	// length. It never has a byte of the value beyond that prefix.
	Preview string
	// Ref is the secret ref that would replace it: deterministic and valid
	// for domain.ValidSecretRef.
	Ref string
	// Replaceable says whether the value can be replaced by its marker.
	Replaceable bool
	// Reason is why not, when it is not.
	Reason string

	start, end int // byte range of the value in the content of File
}

// Report is what a snapshot carries in the way of secrets.
type Report struct {
	Substituted []Substituted
	Hits        []Hit
}

// Scan reports the refs the snapshot already has and the secret-looking
// values it still carries. It does not modify the snapshot.
func Scan(snap harness.NativeSnapshot) Report {
	var r Report
	used := map[string]bool{}
	for _, c := range snap.Components {
		for _, f := range c.Files {
			for _, ref := range harness.SecretRefs(f.Content) {
				r.Substituted = append(r.Substituted, Substituted{Component: c.Name, File: f.Path, Ref: ref})
				used[ref] = true
			}
		}
	}
	for _, c := range snap.Components {
		for _, f := range c.Files {
			for _, h := range scanFile(f.Content) {
				h.Component, h.File = c.Name, f.Path
				h.Ref = uniqueRef(snap.Harness, c.Name, f.Path, h.key, h.Kind, used)
				r.Hits = append(r.Hits, h.Hit)
			}
		}
	}
	return r
}

// Apply replaces every hit of the snapshot with its marker, and recomputes the
// SecretRefs of each file and the Digest of each component it changed. A hit
// that cannot be replaced is an error and nothing is returned. The input is
// never modified.
func Apply(snap harness.NativeSnapshot) (harness.NativeSnapshot, error) {
	report := Scan(snap)
	for _, h := range report.Hits {
		if !h.Replaceable {
			return harness.NativeSnapshot{}, fmt.Errorf("%w: %s", ErrNotReplaceable, h.where())
		}
	}

	byFile := map[[2]string][]Hit{}
	for _, h := range report.Hits {
		k := [2]string{h.Component, h.File}
		byFile[k] = append(byFile[k], h)
	}
	out := snap
	out.Components = make([]Component, 0, len(snap.Components))
	for _, c := range snap.Components {
		nc := c
		nc.Files = make([]harness.File, len(c.Files))
		changed := false
		for i, f := range c.Files {
			nf := f
			nf.Content = slices.Clone(f.Content)
			nf.SecretRefs = slices.Clone(f.SecretRefs)
			if hits := byFile[[2]string{c.Name, f.Path}]; len(hits) > 0 {
				nf.Content = replace(f.Content, hits)
				nf.SecretRefs = harness.SecretRefs(nf.Content)
				changed = true
			}
			nc.Files[i] = nf
		}
		if changed {
			d, err := harness.FilesDigest(nc.Files)
			if err != nil {
				return harness.NativeSnapshot{}, fmt.Errorf("workflowadopt: digest of component %s: %w", c.Name, err)
			}
			nc.Digest = d
		}
		out.Components = append(out.Components, nc)
	}
	return out, nil
}

// Component aliases the harness type so the signatures above read plainly.
type Component = harness.Component

func (h Hit) where() string {
	return fmt.Sprintf("%s: %s line %d (%s)", h.Component, h.File, h.Line, h.Kind)
}

// replace writes the marker of each hit over its range; hits are in order.
func replace(content []byte, hits []Hit) []byte {
	var out []byte
	last := 0
	for _, h := range hits {
		out = append(out, content[last:h.start]...)
		out = append(out, harness.SecretMarker(h.Ref)...)
		last = h.end
	}
	return append(out, content[last:]...)
}

// --- detection -------------------------------------------------------------

type fileHit struct {
	Hit
	key string // the name the value is assigned to, if any
}

type pattern struct {
	kind   string
	prefix string // fixed literal prefix, safe to show
	re     *regexp.Regexp
	// ok is an extra check on the value; nil is none.
	ok func(v string) bool
	// fixedLen: the value must not be followed by one more token character.
	fixedLen bool
}

// lead is the byte before a token: a token never starts inside a word.
const lead = `(?:^|[^A-Za-z0-9_-])`

var patterns = []pattern{
	{kind: "github-token", prefix: "gh", re: regexp.MustCompile(lead + `(gh[pousr]_[A-Za-z0-9]{36,255})`)},
	{kind: "github-token", prefix: "github_pat_", re: regexp.MustCompile(lead + `(github_pat_[A-Za-z0-9_]{22,255})`)},
	{kind: "anthropic-key", prefix: "sk-ant-", re: regexp.MustCompile(lead + `(sk-ant-[A-Za-z0-9_-]{20,})`), ok: mixed},
	{kind: "openai-key", prefix: "sk-", re: regexp.MustCompile(lead + `(sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,})`), ok: mixed},
	{kind: "aws-access-key", prefix: "AK", re: regexp.MustCompile(lead + `((?:AKIA|ASIA)[A-Z0-9]{16})`), fixedLen: true},
	{kind: "slack-token", prefix: "xox", re: regexp.MustCompile(lead + `(xox[abprs]-[A-Za-z0-9-]{10,})`), ok: mixed},
	{kind: "google-api-key", prefix: "AIza", re: regexp.MustCompile(lead + `(AIza[A-Za-z0-9_-]{35})`), fixedLen: true},
	// The password of scheme://user:password@host, with or without a user
	// (redis://:password@host). In that position the shape alone says it is a
	// password, so there is no length floor: only a reference or a placeholder
	// is let through.
	{kind: "url-credentials", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@"'<>]*:([^\s/@"'<>]+)@`), ok: notReference},
	// A credential in the query of a URL.
	{kind: "url-token", re: regexp.MustCompile(`(?i)[?&](?:access_token|auth_token|id_token|refresh_token|token|api_key|apikey|key|client_secret|secret|password)=([^&\s"'#<>]+)`), ok: credential},
	// The value of a Bearer header, or of a Basic one under Authorization.
	{kind: "authorization-header", re: regexp.MustCompile(`(?i)(?:\bbearer|authorization["']?[ \t]*[=:][ \t]*["']?basic)[ \t]+([A-Za-z0-9._~+/-]{16,}=*)`), ok: mixed},
}

var (
	pem = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	// generic: NAME = value, NAME: value, "NAME": "value", where NAME has
	// key, token, secret or password in it.
	generic = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:key|token|secret|passw(?:or)?d)[A-Za-z0-9_.-]*)["']?[ \t]*[=:][ \t]*["']?([^\s"',;\\]+)`)
	// argvFlag: "--api-key", "value" as two strings of an args list. Only a
	// flag whose name has key, token, secret or password in it; the value is
	// the next string. A short flag ("-k") says nothing about what it takes.
	argvFlag = regexp.MustCompile(`(?i)["'](-{1,2}[A-Za-z0-9_.-]*(?:key|token|secret|passw(?:or)?d)[A-Za-z0-9_.-]*)["'][ \t]*,[ \t\r\n]*["']([^"'\s]+)["']`)
	// not a value: the name of a variable or a file, not a secret.
	namesAnother = regexp.MustCompile(`(?i)([_-]env|[_-]env[_-]var|[_-]file|[_-]path|[_-]name|[_-]url)$`)
	namesSecret  = regexp.MustCompile(`(?i)token|secret|passw`)
	uuid         = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexOnly      = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	markerRe     = regexp.MustCompile(`\{\{cf-secret:[^{}]*\}\}`)
	argsOpen     = regexp.MustCompile(`(?i)\bargs["']?[ \t]*[=:][ \t]*\[`)
	keyBefore    = regexp.MustCompile(`([A-Za-z0-9_.-]+)["']?[ \t]*[=:][ \t]*["']?$`)
)

func scanFile(content []byte) []fileHit {
	// Markers already there are not findings: blank them, same length.
	text := string(markerRe.ReplaceAllFunc(content, func(m []byte) []byte {
		return []byte(strings.Repeat(" ", len(m)))
	}))
	var hits []fileHit
	overlaps := func(s, e int) bool {
		return slices.ContainsFunc(hits, func(h fileHit) bool { return s < h.end && h.start < e })
	}
	add := func(text string, s, e int, kind, prefix string) {
		h := fileHit{key: keyOf(text, s)}
		h.start, h.end, h.Kind = s, e, kind
		h.Line = 1 + strings.Count(text[:s], "\n")
		h.Preview = fmt.Sprintf("%s…(%d chars)", prefix, e-s)
		h.Replaceable = true
		if inArgs(text, s) {
			h.Replaceable, h.Reason = false, "inside the args of a command: arguments cannot be told apart into secret and not; pass it through env"
		}
		hits = append(hits, h)
	}

	for _, loc := range pem.FindAllStringIndex(text, -1) {
		add(text, loc[0], loc[1], "private-key", "-----BEGIN PRIVATE KEY-----")
	}
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2], m[3]
			if overlaps(s, e) || (p.fixedLen && e < len(text) && isAlnum(text[e])) {
				continue
			}
			if p.ok != nil && !p.ok(text[s:e]) {
				continue
			}
			add(text, s, e, p.kind, p.prefix)
		}
	}
	for _, m := range generic.FindAllStringSubmatchIndex(text, -1) {
		key, s, e := text[m[2]:m[3]], m[4], m[5]
		if namesAnother.MatchString(key) || overlaps(s, e) || !looksSecret(key, text[s:e]) {
			continue
		}
		add(text, s, e, "assigned-secret", key+"=")
	}
	for _, m := range argvFlag.FindAllStringSubmatchIndex(text, -1) {
		flag, s, e := text[m[2]:m[3]], m[4], m[5]
		if namesAnother.MatchString(flag) || overlaps(s, e) || !looksSecret(flag, text[s:e]) {
			continue
		}
		add(text, s, e, "assigned-secret", flag+" ")
	}
	slices.SortFunc(hits, func(a, b fileHit) int { return a.start - b.start })
	return hits
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// credential: a value that is not a reference or a placeholder.
func credential(v string) bool {
	return len(v) >= 8 && !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "{{") && !placeholder(v)
}

// notReference: a value that is not a reference or a placeholder, of any length.
func notReference(v string) bool {
	return !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "{{") && !placeholder(v)
}

func placeholder(v string) bool {
	lv := strings.ToLower(v)
	for _, w := range []string{"example", "changeme", "placeholder", "your", "xxxx", "redacted"} {
		if strings.Contains(lv, w) {
			return true
		}
	}
	return false
}

// mixed: a prose word that begins like a key ("sk-") has no digits.
func mixed(v string) bool { return strings.ContainsAny(v, "0123456789") && entropy(v) >= 3.0 }

// looksSecret is the filter of a generic assignment to key, biased against
// false positives: long, with letters and digits, high entropy, and not an
// identifier, path, URL, UUID, placeholder or reference. A 40 or 64 digit hex
// value is a git object ID or a digest unless the key says it is a secret.
func looksSecret(key, v string) bool {
	switch {
	case len(v) < 16,
		strings.HasPrefix(v, "{{"), strings.HasPrefix(v, "$"),
		strings.HasPrefix(v, "/"), strings.HasPrefix(v, "./"), strings.HasPrefix(v, "../"), strings.HasPrefix(v, "~"),
		strings.Contains(v, "://"), strings.ContainsAny(v, "<>"),
		uuid.MatchString(v),
		hexOnly.MatchString(v) && (len(v) == 40 || len(v) == 64) && !namesSecret.MatchString(key):
		return false
	}
	if placeholder(v) {
		return false
	}
	hasLetter := strings.IndexFunc(v, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }) >= 0
	return hasLetter && strings.ContainsAny(v, "0123456789") && entropy(v) >= 3.0
}

func entropy(s string) float64 {
	var freq [256]float64
	for i := range len(s) {
		freq[s[i]]++
	}
	var h float64
	for _, n := range freq {
		if n > 0 {
			p := n / float64(len(s))
			h -= p * math.Log2(p)
		}
	}
	return h
}

// keyOf is the name the value at off is assigned to, "" if none.
func keyOf(text string, off int) string {
	ls := strings.LastIndexByte(text[:off], '\n') + 1
	if m := keyBefore.FindStringSubmatch(text[ls:off]); m != nil {
		return m[1]
	}
	return ""
}

// inArgs says whether off is inside the array of an `args` key: after its
// '[' and before the matching ']' (brackets inside strings do not count).
func inArgs(text string, off int) bool {
	locs := argsOpen.FindAllStringIndex(text[:off], -1)
	if len(locs) == 0 {
		return false
	}
	depth, quote := 0, byte(0)
	for i := locs[len(locs)-1][1] - 1; i < off; i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
		}
	}
	return depth > 0
}

// --- refs ------------------------------------------------------------------

// uniqueRef is adopt/<harness>/<component>/<file>/<key or kind>, with a
// numeric suffix when taken. It depends on the position of the secret, never
// on its value.
func uniqueRef(harnessID, component, file, key, kind string, used map[string]bool) string {
	name := key
	if name == "" {
		name = kind
	}
	ref := strings.Join([]string{"adopt", refSegment(harnessID), refSegment(component), refSegment(file), refSegment(name)}, "/")
	if len(ref) > domain.MaxSecretRefLen {
		sum := sha256.Sum256([]byte(component + "\x00" + file))
		short := refSegment(name)
		short = short[:min(len(short), 40)]
		ref = "adopt/" + hex.EncodeToString(sum[:6]) + "/" + short
	}
	base := ref
	for n := 2; used[ref]; n++ {
		ref = fmt.Sprintf("%s-%d", base, n)
	}
	used[ref] = true
	return ref
}

func refSegment(s string) string {
	out := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
	if out == "" {
		return "_"
	}
	return out
}
