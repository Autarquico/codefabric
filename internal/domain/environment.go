package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// ErrInvalidEnvironment wraps every violation EnvironmentDef.Validate reports.
var ErrInvalidEnvironment = errors.New("invalid environment")

// ErrInvalidEnvironmentRef reports text that is not an Environment reference.
var ErrInvalidEnvironmentRef = errors.New("invalid environment reference")

// ProviderHost is the only Environment provider this code runs (§25.3): the
// scripts run on the Worker's own machine.
const ProviderHost = "host"

// Bounds of an Environment, so that a definition cannot cost more to store,
// freeze in a spec or run than an Environment ever does.
const (
	MaxEnvironmentServices     = 32
	MaxEnvironmentDigestInputs = 256
	MaxEnvironmentCommandLen   = 4 << 10
)

// EnvironmentDef is the definition of an Environment (§25.3). Name and version
// are identity, kept apart so that they never change the digest.
type EnvironmentDef struct {
	Provider string `json:"provider"`
	// RequiresCapabilities feeds the eligibility filter of the scheduler.
	RequiresCapabilities []string `json:"requires_capabilities"`
	// Prepare is the command that runs in every new worktree.
	Prepare  string               `json:"prepare"`
	Services []EnvironmentService `json:"services"`
	// DigestInputs are paths in the repo, relative and with slashes, whose
	// content is part of the effective digest.
	DigestInputs []string `json:"digest_inputs"`
}

// EnvironmentService is a process the Run needs while it lives. Stop and Ready
// are optional.
type EnvironmentService struct {
	Name  string `json:"name"`
	Start string `json:"start"`
	Stop  string `json:"stop"`
	Ready string `json:"ready"`
}

// Validate checks the definition in the shape the Controller persists.
func (d EnvironmentDef) Validate() error {
	if d.Provider != ProviderHost {
		return invalidEnvironment("provider %q is not supported (only %q)", d.Provider, ProviderHost)
	}
	for _, c := range d.RequiresCapabilities {
		if c == "" || strings.ContainsAny(c, " \t\r\n") {
			return invalidEnvironment("requires_capabilities: %q is not a capability", c)
		}
	}
	if err := validCommand("prepare", d.Prepare); err != nil {
		return err
	}
	if len(d.Services) > MaxEnvironmentServices {
		return invalidEnvironment("more than %d services", MaxEnvironmentServices)
	}
	names := map[string]bool{}
	for _, s := range d.Services {
		if !token.MatchString(s.Name) {
			return invalidEnvironment("service name %q is not a token", s.Name)
		}
		if names[s.Name] {
			return invalidEnvironment("service %q is declared twice", s.Name)
		}
		names[s.Name] = true
		if s.Start == "" {
			return invalidEnvironment("service %q: start is empty", s.Name)
		}
		for field, cmd := range map[string]string{"start": s.Start, "stop": s.Stop, "ready": s.Ready} {
			if err := validCommand("service "+s.Name+": "+field, cmd); err != nil {
				return err
			}
		}
	}
	if len(d.DigestInputs) > MaxEnvironmentDigestInputs {
		return invalidEnvironment("more than %d digest_inputs", MaxEnvironmentDigestInputs)
	}
	seen := map[string]bool{}
	for _, p := range d.DigestInputs {
		if !validDigestInput(p) {
			return invalidEnvironment("digest_inputs: %q is not a relative path with slashes", p)
		}
		if seen[p] {
			return invalidEnvironment("digest_inputs: %q is listed twice", p)
		}
		seen[p] = true
	}
	return nil
}

func validCommand(field, cmd string) error {
	if len(cmd) > MaxEnvironmentCommandLen {
		return invalidEnvironment("%s is longer than %d bytes", field, MaxEnvironmentCommandLen)
	}
	return nil
}

// validDigestInput accepts clean relative paths: no '..', no leading or
// trailing slash, no '.' or empty segments, no backslash or NUL.
func validDigestInput(p string) bool {
	return p != "" && !strings.ContainsAny(p, "\\\x00") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

func invalidEnvironment(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEnvironment, fmt.Sprintf(format, args...))
}

// Canonical is the deterministic JSON of the definition. Services keep the
// order they were declared in, which is the order they start in; the
// capabilities and the digest inputs are sets and come out sorted. Absent
// lists are empty ones, so that both spell the same bytes.
func (d EnvironmentDef) Canonical() ([]byte, error) {
	c := d
	c.RequiresCapabilities = sortedCopy(d.RequiresCapabilities)
	c.DigestInputs = sortedCopy(d.DigestInputs)
	c.Services = append([]EnvironmentService{}, d.Services...)
	return json.Marshal(c)
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	slices.Sort(out)
	return out
}

// Digest is "sha256:<hex>" of the canonical JSON: the identity of the
// definition a Run pins. It does not read the repo; the content of the digest
// inputs belongs to the effective digest.
func (d EnvironmentDef) Digest() (string, error) {
	canonical, err := d.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EnvironmentSelector is the parsed text form of a reference; it points at an
// Environment, and optionally at one of its versions. Exactly one of Latest,
// Version and Digest is set when a version is named; none is set for a bare
// name, which callers read as "latest".
type EnvironmentSelector struct {
	Name    string
	Latest  bool
	Version int
	Digest  string
}

// ParseEnvironmentRef accepts `name`, `name@latest`, `name@<N>` (N >= 1, no
// leading zeros) and `name@sha256:<hex>`.
func ParseEnvironmentRef(text string) (EnvironmentSelector, error) {
	name, version, hasVersion := strings.Cut(text, "@")
	if !ValidSlug(name) {
		return EnvironmentSelector{}, fmt.Errorf("%w: %q", ErrInvalidEnvironmentRef, text)
	}
	ref := EnvironmentSelector{Name: name}
	if !hasVersion {
		return ref, nil
	}
	if version == "latest" {
		ref.Latest = true
		return ref, nil
	}
	if digestForm.MatchString(version) {
		ref.Digest = version
		return ref, nil
	}
	n, err := strconv.Atoi(version)
	if err != nil || n < 1 || strconv.Itoa(n) != version {
		return EnvironmentSelector{}, fmt.Errorf("%w: %q", ErrInvalidEnvironmentRef, text)
	}
	ref.Version = n
	return ref, nil
}

// Origin says where the value of a field of the effective configuration comes
// from (§3.1); Clamped is a value the repo asked for and the Controller
// lowered.
type Origin string

// Origins of a field, from the widest default to the narrowest override.
const (
	OriginGlobal  Origin = "global"
	OriginProject Origin = "project"
	OriginTask    Origin = "task"
	OriginRepo    Origin = "repo"
	OriginClamped Origin = "clamped"
)
