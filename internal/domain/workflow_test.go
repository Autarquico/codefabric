package domain

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

func hexDigest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func sampleManifest() Manifest {
	return Manifest{SchemaVersion: 1, Components: []ManifestComponent{
		{Harness: "codex", Name: "skills", Kind: "dir", Ownership: OwnershipManaged, TreeDigest: hexDigest('a'), Files: []ManifestFile{
			{Path: "skills/b.md", SHA256: hexDigest('1')},
			{Path: "skills/a.md", Exec: true, SHA256: hexDigest('2'), SecretRefs: []string{"gh-token", "api-key"}},
		}},
		{Harness: "claude", Name: "settings", Kind: "file", Ownership: OwnershipLocal, TreeDigest: hexDigest('b'), Files: []ManifestFile{
			{Path: "settings.json", SHA256: hexDigest('3')},
		}},
	}}
}

func mustDigest(t *testing.T, m Manifest) string {
	t.Helper()
	d, err := m.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	return d
}

func TestScenario_DigestEstable(t *testing.T) {
	a, b := sampleManifest(), sampleManifest()
	if mustDigest(t, a) != mustDigest(t, b) {
		t.Fatal("same content gave different digests")
	}
	if d := mustDigest(t, a); !digestForm.MatchString(d) {
		t.Fatalf("digest %q is not sha256:<hex>", d)
	}
	b.Components[0], b.Components[1] = b.Components[1], b.Components[0]
	b.Components[1].Files[0], b.Components[1].Files[1] = b.Components[1].Files[1], b.Components[1].Files[0]
	if mustDigest(t, a) != mustDigest(t, b) {
		t.Fatal("component and file order changed the digest")
	}
}

func TestDigest_ChangesWithCoveredFields(t *testing.T) {
	base := mustDigest(t, sampleManifest())
	mutations := map[string]func(*Manifest){
		"harness":     func(m *Manifest) { m.Components[0].Harness = "other" },
		"name":        func(m *Manifest) { m.Components[0].Name = "other" },
		"kind":        func(m *Manifest) { m.Components[0].Kind = "file" },
		"ownership":   func(m *Manifest) { m.Components[0].Ownership = OwnershipLocal },
		"tree digest": func(m *Manifest) { m.Components[0].TreeDigest = hexDigest('c') },
		"path":        func(m *Manifest) { m.Components[0].Files[0].Path = "skills/z.md" },
		"exec":        func(m *Manifest) { m.Components[0].Files[0].Exec = true },
		"sha256":      func(m *Manifest) { m.Components[0].Files[0].SHA256 = hexDigest('9') },
		"secret ref":  func(m *Manifest) { m.Components[0].Files[1].SecretRefs = []string{"api-key"} },
		"component":   func(m *Manifest) { m.Components = m.Components[:1] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			m := sampleManifest()
			mutate(&m)
			if mustDigest(t, m) == base {
				t.Fatalf("changing %s kept the digest", name)
			}
		})
	}
}

func TestDigest_DoesNotMutateTheManifest(t *testing.T) {
	m := sampleManifest()
	mustDigest(t, m)
	if m.Components[0].Harness != "codex" || m.Components[0].Files[0].Path != "skills/b.md" ||
		m.Components[0].Files[1].SecretRefs[0] != "gh-token" {
		t.Fatalf("Digest reordered the caller's manifest: %+v", m)
	}
}

func TestProperty_Digest_OrderDoesNotMatter(t *testing.T) {
	seed := rand.Uint64()                  //nolint:gosec // a seed to log, not a secret
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // as above
	for n := range 100 {
		m := randomManifest(r)
		want := mustDigest(t, m)
		shuffled := Manifest{SchemaVersion: m.SchemaVersion, Components: append([]ManifestComponent(nil), m.Components...)}
		r.Shuffle(len(shuffled.Components), func(i, j int) {
			shuffled.Components[i], shuffled.Components[j] = shuffled.Components[j], shuffled.Components[i]
		})
		for i, c := range shuffled.Components {
			c.Files = append([]ManifestFile(nil), c.Files...)
			r.Shuffle(len(c.Files), func(a, b int) { c.Files[a], c.Files[b] = c.Files[b], c.Files[a] })
			shuffled.Components[i] = c
		}
		if got := mustDigest(t, shuffled); got != want {
			t.Fatalf("tree %d (seed %#x): shuffling changed the digest", n, seed)
		}
	}
}

func randomManifest(r *rand.Rand) Manifest {
	m := Manifest{SchemaVersion: 1}
	for i := range 1 + r.IntN(5) {
		c := ManifestComponent{
			Harness: []string{"claude", "codex"}[r.IntN(2)], Name: "c" + string(rune('a'+i)), Kind: "dir",
			Ownership: OwnershipManaged, TreeDigest: hexDigest("abcdef"[r.IntN(6)]),
		}
		for j := range 1 + r.IntN(6) {
			f := ManifestFile{Path: "d/f" + string(rune('a'+j)), Exec: r.IntN(2) == 0, SHA256: hexDigest("abcdef"[r.IntN(6)])}
			f.SecretRefs = append(f.SecretRefs, []string{"one", "two", "three"}[:r.IntN(4)]...)
			c.Files = append(c.Files, f)
		}
		m.Components = append(m.Components, c)
	}
	return m
}

func TestCanonical_IsStable(t *testing.T) {
	got, err := sampleManifest().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema_version":1,"components":[` +
		`{"harness":"claude","name":"settings","kind":"file","ownership":"local","files":[` +
		`{"path":"settings.json","exec":false,"sha256":"sha256:3333333333333333333333333333333333333333333333333333333333333333","secret_refs":[]}],` +
		`"tree_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},` +
		`{"harness":"codex","name":"skills","kind":"dir","ownership":"managed","files":[` +
		`{"path":"skills/a.md","exec":true,"sha256":"sha256:2222222222222222222222222222222222222222222222222222222222222222","secret_refs":["api-key","gh-token"]},` +
		`{"path":"skills/b.md","exec":false,"sha256":"sha256:1111111111111111111111111111111111111111111111111111111111111111","secret_refs":[]}],` +
		`"tree_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	if string(got) != want {
		t.Fatalf("Canonical =\n%s\nwant\n%s", got, want)
	}
}

func TestManifestValidate(t *testing.T) {
	tests := map[string]func(*Manifest){
		"valid":                     func(*Manifest) {},
		"schema version":            func(m *Manifest) { m.SchemaVersion = 2 },
		"no components":             func(m *Manifest) { m.Components = nil },
		"duplicate component":       func(m *Manifest) { m.Components[1].Harness, m.Components[1].Name = "codex", "skills" },
		"same name other harness":   func(m *Manifest) { m.Components[1].Name = "skills" },
		"bad harness":               func(m *Manifest) { m.Components[0].Harness = "Codex" },
		"empty name":                func(m *Manifest) { m.Components[0].Name = "" },
		"file name":                 func(m *Manifest) { m.Components[0].Name = "AGENTS.md" },
		"path name":                 func(m *Manifest) { m.Components[0].Name = "skills/review" },
		"dot dot name":              func(m *Manifest) { m.Components[0].Name = "../skills" },
		"absolute name":             func(m *Manifest) { m.Components[0].Name = "/skills" },
		"name with a space":         func(m *Manifest) { m.Components[0].Name = "my skills" },
		"empty kind":                func(m *Manifest) { m.Components[0].Kind = "" },
		"merged ownership":          func(m *Manifest) { m.Components[0].Ownership = OwnershipMerged },
		"ignored ownership":         func(m *Manifest) { m.Components[0].Ownership = OwnershipIgnored },
		"empty ownership":           func(m *Manifest) { m.Components[0].Ownership = "" },
		"short tree digest":         func(m *Manifest) { m.Components[0].TreeDigest = "sha256:abc" },
		"upper-case tree digest":    func(m *Manifest) { m.Components[0].TreeDigest = "sha256:" + strings.Repeat("A", 64) },
		"no files":                  func(m *Manifest) { m.Components[0].Files = nil },
		"duplicate path":            func(m *Manifest) { m.Components[0].Files[1].Path = "skills/b.md" },
		"same path other component": func(m *Manifest) { m.Components[1].Files[0].Path = "skills/b.md" },
		"absolute path":             func(m *Manifest) { m.Components[0].Files[0].Path = "/etc/passwd" },
		"dot dot":                   func(m *Manifest) { m.Components[0].Files[0].Path = "../x" },
		"dot segment":               func(m *Manifest) { m.Components[0].Files[0].Path = "a/./b" },
		"trailing slash":            func(m *Manifest) { m.Components[0].Files[0].Path = "a/" },
		"backslash is a name":       func(m *Manifest) { m.Components[0].Files[0].Path = `a\b` },
		"root":                      func(m *Manifest) { m.Components[0].Files[0].Path = "." },
		"empty path":                func(m *Manifest) { m.Components[0].Files[0].Path = "" },
		"bad sha256":                func(m *Manifest) { m.Components[0].Files[0].SHA256 = "abc" },
		"no prefix sha256":          func(m *Manifest) { m.Components[0].Files[0].SHA256 = strings.Repeat("a", 64) },
		"bad secret ref":            func(m *Manifest) { m.Components[0].Files[0].SecretRefs = []string{"a b"} },
		"repeated secret ref":       func(m *Manifest) { m.Components[0].Files[0].SecretRefs = []string{"k", "k"} },
	}
	wantValid := map[string]bool{"valid": true, "file name": true, "path name": true, "same name other harness": true, "same path other component": true, "backslash is a name": true}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := sampleManifest()
			mutate(&m)
			err := m.Validate()
			if wantValid[name] {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("Validate = %v, want ErrInvalidManifest", err)
			}
			if _, derr := m.Digest(); derr == nil {
				t.Fatal("Digest accepted an invalid manifest")
			}
		})
	}
}

func TestParseWorkflowRef(t *testing.T) {
	d := hexDigest('a')
	tests := []struct {
		in   string
		want WorkflowSelector
		ok   bool
	}{
		{"personal", WorkflowSelector{Name: "personal"}, true},
		{"personal@latest", WorkflowSelector{Name: "personal", Latest: true}, true},
		{"personal@17", WorkflowSelector{Name: "personal", Version: 17}, true},
		{"my.flow_2@1", WorkflowSelector{Name: "my.flow_2", Version: 1}, true},
		{"personal@" + d, WorkflowSelector{Name: "personal", Digest: d}, true},
		{"", WorkflowSelector{}, false},
		{"@1", WorkflowSelector{}, false},
		{"personal@", WorkflowSelector{}, false},
		{"personal@0", WorkflowSelector{}, false},
		{"personal@-1", WorkflowSelector{}, false},
		{"personal@+1", WorkflowSelector{}, false},
		{"personal@017", WorkflowSelector{}, false},
		{"personal@1.5", WorkflowSelector{}, false},
		{"personal@99999999999999999999", WorkflowSelector{}, false},
		{"personal@Latest", WorkflowSelector{}, false},
		{"personal@latest@1", WorkflowSelector{}, false},
		{"personal@sha256:abc", WorkflowSelector{}, false},
		{"personal@sha256:" + strings.Repeat("A", 64), WorkflowSelector{}, false},
		{"personal@md5:" + strings.Repeat("a", 32), WorkflowSelector{}, false},
		{"Personal", WorkflowSelector{}, false},
		{"my flow", WorkflowSelector{}, false},
		{"-x", WorkflowSelector{}, false},
		{"a/b", WorkflowSelector{}, false},
		{strings.Repeat("a", 64), WorkflowSelector{}, false},
		{" personal", WorkflowSelector{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseWorkflowRef(tt.in)
			if tt.ok != (err == nil) || (!tt.ok && !errors.Is(err, ErrInvalidWorkflowRef)) {
				t.Fatalf("ParseWorkflowRef(%q) error = %v, want ok=%v", tt.in, err, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("ParseWorkflowRef(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
