package domain

import (
	"errors"
	"strings"
	"testing"
)

func validEnv() EnvironmentDef {
	return EnvironmentDef{
		Provider:             "host",
		RequiresCapabilities: []string{"docker", "git"},
		Prepare:              "./scripts/cf-prepare.sh",
		Services:             []EnvironmentService{{Name: "postgres", Start: "docker compose up -d db", Stop: "docker compose stop db", Ready: "pg_isready"}},
		DigestInputs:         []string{"mise.toml", "go.sum"},
	}
}

func TestEnvironmentDef_Validate(t *testing.T) {
	if err := validEnv().Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	cases := map[string]func(*EnvironmentDef){
		"provider":        func(d *EnvironmentDef) { d.Provider = "docker" },
		"empty provider":  func(d *EnvironmentDef) { d.Provider = "" },
		"capability":      func(d *EnvironmentDef) { d.RequiresCapabilities = []string{"a b"} },
		"service name":    func(d *EnvironmentDef) { d.Services[0].Name = "Pg" },
		"duplicate svc":   func(d *EnvironmentDef) { d.Services = append(d.Services, d.Services[0]) },
		"empty start":     func(d *EnvironmentDef) { d.Services[0].Start = "" },
		"long prepare":    func(d *EnvironmentDef) { d.Prepare = strings.Repeat("x", MaxEnvironmentCommandLen+1) },
		"too many svc":    func(d *EnvironmentDef) { d.Services = make([]EnvironmentService, MaxEnvironmentServices+1) },
		"absolute input":  func(d *EnvironmentDef) { d.DigestInputs = []string{"/etc/passwd"} },
		"parent input":    func(d *EnvironmentDef) { d.DigestInputs = []string{"../x"} },
		"dot input":       func(d *EnvironmentDef) { d.DigestInputs = []string{"a/./b"} },
		"backslash input": func(d *EnvironmentDef) { d.DigestInputs = []string{`a\b`} },
		"duplicate input": func(d *EnvironmentDef) { d.DigestInputs = []string{"a", "a"} },
		"too many inputs": func(d *EnvironmentDef) { d.DigestInputs = make([]string, MaxEnvironmentDigestInputs+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validEnv()
			d.Services = append([]EnvironmentService(nil), d.Services...)
			mutate(&d)
			if err := d.Validate(); !errors.Is(err, ErrInvalidEnvironment) {
				t.Fatalf("got %v, want ErrInvalidEnvironment", err)
			}
		})
	}
}

func TestEnvironmentDef_DigestIgnoresSetOrderAndNilLists(t *testing.T) {
	a := validEnv()
	b := validEnv()
	b.RequiresCapabilities = []string{"git", "docker"}
	b.DigestInputs = []string{"go.sum", "mise.toml"}
	da, _ := a.Digest()
	db, _ := b.Digest()
	if da != db || !digestForm.MatchString(da) {
		t.Fatalf("digests %q, %q", da, db)
	}
	empty, nilled := EnvironmentDef{Provider: "host", RequiresCapabilities: []string{}}, EnvironmentDef{Provider: "host"}
	de, _ := empty.Digest()
	dn, _ := nilled.Digest()
	if de != dn {
		t.Fatalf("nil and empty lists differ: %q vs %q", de, dn)
	}
}

func TestEnvironmentDef_DigestSeesServiceOrderAndContent(t *testing.T) {
	a := validEnv()
	a.Services = append(a.Services, EnvironmentService{Name: "redis", Start: "redis-server"})
	b := a
	b.Services = []EnvironmentService{a.Services[1], a.Services[0]}
	c := validEnv()
	c.Prepare = "other"
	da, _ := a.Digest()
	db, _ := b.Digest()
	dc, _ := c.Digest()
	base, _ := validEnv().Digest()
	if da == db || dc == base {
		t.Fatal("service order and content must change the digest")
	}
}

func TestEnvironmentDef_CanonicalDoesNotMutate(t *testing.T) {
	d := validEnv()
	d.DigestInputs = []string{"z", "a"}
	if _, err := d.Canonical(); err != nil {
		t.Fatal(err)
	}
	if d.DigestInputs[0] != "z" {
		t.Fatal("Canonical sorted the caller's slice")
	}
}

func TestParseEnvironmentRef(t *testing.T) {
	good := map[string]EnvironmentSelector{
		"backend":        {Name: "backend"},
		"backend@latest": {Name: "backend", Latest: true},
		"backend@12":     {Name: "backend", Version: 12},
		"backend@sha256:" + strings.Repeat("a", 64): {Name: "backend", Digest: "sha256:" + strings.Repeat("a", 64)},
	}
	for text, want := range good {
		got, err := ParseEnvironmentRef(text)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v", text, got, err)
		}
	}
	for _, text := range []string{"", "Back", "backend@", "backend@0", "backend@012", "backend@-1", "backend@sha256:abc", "backend@v1"} {
		if _, err := ParseEnvironmentRef(text); !errors.Is(err, ErrInvalidEnvironmentRef) {
			t.Errorf("%q: got %v, want ErrInvalidEnvironmentRef", text, err)
		}
	}
}

func TestRunSpecSchemaWith(t *testing.T) {
	cases := []struct {
		secrets, repo bool
		want          int
	}{{false, false, 2}, {true, false, 3}, {false, true, 4}, {true, true, 4}}
	for _, c := range cases {
		if got := RunSpecSchemaWith(c.secrets, c.repo); got != c.want {
			t.Errorf("secrets=%v repo=%v: got %d, want %d", c.secrets, c.repo, got, c.want)
		}
	}
	for schema, want := range map[int]bool{1: false, 2: true, 3: true, 4: true, 5: false} {
		if ReadableRunSpecSchema(schema) != want {
			t.Errorf("ReadableRunSpecSchema(%d) != %v", schema, want)
		}
	}
}
