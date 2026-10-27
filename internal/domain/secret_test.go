package domain

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// harnessRefGrammar is the grammar the {{cf-secret:<ref>}} markers of the
// harness port have always used: ValidSecretRef must accept exactly it.
var harnessRefGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func TestValidSecretRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"github-token": true, "a": true, "9": true, "team/ci.deploy_key-2": true,
		strings.Repeat("a", MaxSecretRefLen): true,

		"": false, strings.Repeat("a", MaxSecretRefLen+1): false,
		"-x": false, ".x": false, "/x": false, "_x": false,
		"github/*": false, "a?": false, "a[1]": false, "a b": false, "a:b": false,
		"a}}": false, "ñ": false, "a\n": false, "a\x00": false,
	} {
		if got := ValidSecretRef(ref); got != want {
			t.Errorf("ValidSecretRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestValidSecretRefIsTheMarkerGrammar(t *testing.T) {
	alphabet := "aZ09._/-*? {}:\\\x00é"
	var refs []string
	for _, a := range alphabet {
		for _, b := range alphabet {
			refs = append(refs, string(a), string(a)+string(b), "x"+string(a)+string(b))
		}
	}
	refs = append(refs, strings.Repeat("a", 127), strings.Repeat("a", 128), strings.Repeat("a", 129))
	for _, ref := range refs {
		if got, want := ValidSecretRef(ref), harnessRefGrammar.MatchString(ref); got != want {
			t.Errorf("ValidSecretRef(%q) = %v, the marker grammar says %v", ref, got, want)
		}
	}
}

func TestValidSecretEnvName(t *testing.T) {
	for name, want := range map[string]bool{
		"GITHUB_TOKEN": true, "_x": true, "a1": true, "Cf_lower": true, "GITHUB": true, "TMUXED": false,
		strings.Repeat("A", MaxSecretEnvNameLen): true,

		"": false, "1A": false, "A-B": false, "A.B": false, "A B": false, "Ñ": false,
		strings.Repeat("A", MaxSecretEnvNameLen+1): false,
		"CF_TOKEN": false, "GIT_ASKPASS": false, "TMUX": false, "TMUX_PANE": false,
		"PATH": false, "HOME": false, "SHELL": false, "USER": false,
	} {
		if got := ValidSecretEnvName(name); got != want {
			t.Errorf("ValidSecretEnvName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestReservedSecretEnv(t *testing.T) {
	for name, want := range map[string]bool{
		"CF_X": true, "GIT_DIR": true, "TMUX": true, "PATH": true, "USER": true,
		"PATHS": false, "USERNAME": false, "CFX": false, "GITHUB_TOKEN": false,
	} {
		if got := ReservedSecretEnv(name); got != want {
			t.Errorf("ReservedSecretEnv(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCheckSecretRefs(t *testing.T) {
	if err := CheckSecretRefs(nil, 1); err != nil {
		t.Errorf("no refs: %v", err)
	}
	if err := CheckSecretRefs([]string{"a", "b"}, 2); err != nil {
		t.Errorf("two refs: %v", err)
	}
	for name, refs := range map[string][]string{
		"over the limit": {"a", "b", "c"},
		"a glob":         {"github/*"},
		"repeated":       {"a", "a"},
		"empty":          {""},
	} {
		if err := CheckSecretRefs(refs, 2); !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: %v, want ErrInvalidSecret", name, err)
		}
	}
}

func TestCheckSecretEnv(t *testing.T) {
	ok := []SecretEnv{{"A_TOKEN", "github"}, {"B_TOKEN", "github"}}
	if err := CheckSecretEnv(ok); err != nil {
		t.Errorf("two names, one ref: %v", err)
	}
	tooMany := make([]SecretEnv, MaxRunSecrets+1)
	for i := range tooMany {
		tooMany[i] = SecretEnv{Name: "V" + strings.Repeat("X", i), Ref: "r"}
	}
	for name, env := range map[string][]SecretEnv{
		"too many":      tooMany,
		"reserved name": {{"GIT_TOKEN", "github"}},
		"bad name":      {{"1X", "github"}},
		"repeated name": {{"X", "a"}, {"X", "b"}},
		"bad ref":       {{"X", "a*"}},
	} {
		if err := CheckSecretEnv(env); !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: %v, want ErrInvalidSecret", name, err)
		}
	}
}

func TestSecretRefsOf(t *testing.T) {
	got := SecretRefsOf([]SecretEnv{{"A", "z"}, {"B", "a"}, {"C", "z"}})
	if want := []string{"a", "z"}; !slices.Equal(got, want) {
		t.Errorf("SecretRefsOf = %q, want %q", got, want)
	}
	if got := SecretRefsOf(nil); len(got) != 0 {
		t.Errorf("SecretRefsOf(nil) = %q", got)
	}
}

func TestCheckSecretValue(t *testing.T) {
	for n, want := range map[int]bool{
		0: false, MinSecretValueLen - 1: false, MinSecretValueLen: true,
		MaxSecretValueLen: true, MaxSecretValueLen + 1: false,
	} {
		value := []byte(strings.Repeat("s", n))
		err := CheckSecretValue(value)
		if (err == nil) != want {
			t.Errorf("CheckSecretValue(%d bytes) = %v, want ok=%v", n, err, want)
		}
		if err != nil && !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("CheckSecretValue(%d bytes) = %v, want ErrInvalidSecret", n, err)
		}
	}
	if err := CheckSecretValue([]byte("hunter2")); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a short value: %v; it must fail without repeating the value", err)
	}
}

func TestRunSpecSchema(t *testing.T) {
	if got := RunSpecSchemaFor(false); got != RunSpecSchema {
		t.Errorf("without secrets: schema %d, want %d", got, RunSpecSchema)
	}
	if got := RunSpecSchemaFor(true); got != RunSpecSchemaSecrets {
		t.Errorf("with secrets: schema %d, want %d", got, RunSpecSchemaSecrets)
	}
	for schema, want := range map[int]bool{0: false, 1: false, 2: true, 3: true, 4: false} {
		if got := ReadableRunSpecSchema(schema); got != want {
			t.Errorf("ReadableRunSpecSchema(%d) = %v, want %v", schema, got, want)
		}
	}
}
