package harness_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestSecretRefsFindsTheMarkersInOrderOnce(t *testing.T) {
	content := []byte(`{"token": "` + harness.SecretMarker("claude/api-key") + `", ` +
		`"other": "` + harness.SecretMarker("gh.token_2") + `", ` +
		`"again": "` + harness.SecretMarker("claude/api-key") + `"}`)
	if got, want := harness.SecretRefs(content), []string{"claude/api-key", "gh.token_2"}; !slices.Equal(got, want) {
		t.Errorf("SecretRefs = %v, want %v", got, want)
	}
}

func TestSecretRefsIgnoresWhatIsNotAMarker(t *testing.T) {
	for _, s := range []string{
		"",
		"plain text",
		"{{cf-secret:}}",
		"{{cf-secret:-x}}",
		"{{cf-secret:has space}}",
		"{cf-secret:x}",
		"{{cf-secret:x}",
		"{{secret:x}}",
	} {
		if refs := harness.SecretRefs([]byte(s)); len(refs) != 0 {
			t.Errorf("SecretRefs(%q) = %v, want none", s, refs)
		}
	}
}

func TestSecretMarkerRoundTrips(t *testing.T) {
	if got := harness.SecretMarker("a/b"); got != "{{cf-secret:a/b}}" {
		t.Errorf("SecretMarker = %q", got)
	}
	if got := harness.SecretRefs([]byte(harness.SecretMarker("a/b"))); !slices.Equal(got, []string{"a/b"}) {
		t.Errorf("SecretRefs(SecretMarker(a/b)) = %v", got)
	}
}

func TestCheckSecretsRefusesWhatLooksLikeAMarkerAndIsNot(t *testing.T) {
	for _, s := range []string{
		"{{cf-secret:}}",
		"{{cf-secret:../x}}",
		"{{cf-secret: k}}",
		"{{cf-secret:x}",
		"{{cf-secret:" + strings.Repeat("a", 129) + "}}",
		harness.SecretMarker("ok") + " and {{cf-secret:bad ref}}",
	} {
		f := harness.File{Path: "settings.json", Content: []byte(s), SecretRefs: harness.SecretRefs([]byte(s))}
		if err := f.CheckSecrets(); !errors.Is(err, harness.ErrSecretUnavailable) {
			t.Errorf("CheckSecrets(%q) = %v, want ErrSecretUnavailable", s, err)
		}
	}
}

func TestCheckSecretsRefusesSecretRefsThatAreNotThoseOfTheContent(t *testing.T) {
	content := []byte(harness.SecretMarker("a") + harness.SecretMarker("b"))
	for _, refs := range [][]string{nil, {"a"}, {"b", "a"}, {"a", "b", "c"}} {
		f := harness.File{Path: "settings.json", Content: content, SecretRefs: refs}
		if err := f.CheckSecrets(); !errors.Is(err, harness.ErrSecretUnavailable) {
			t.Errorf("CheckSecrets with SecretRefs %q = %v, want ErrSecretUnavailable", refs, err)
		}
	}
	if err := (harness.File{Content: []byte(`{"model":"m1"}`)}).CheckSecrets(); err != nil {
		t.Errorf("CheckSecrets of a file with no markers = %v", err)
	}
	if err := (harness.File{Content: content, SecretRefs: []string{"a", "b"}}).CheckSecrets(); err != nil {
		t.Errorf("CheckSecrets of a consistent file = %v", err)
	}
}

func TestAMarkerIsExactlyARefTheDomainAccepts(t *testing.T) {
	for _, ref := range []string{
		"a", "0", "a/b", "gh.token_2", "x-y", strings.Repeat("a", domain.MaxSecretRefLen),
		"", "-x", ".x", "/x", "../x", "a b", "a{b", "a}b", "a:b", "ñ", strings.Repeat("a", domain.MaxSecretRefLen+1),
	} {
		content := []byte("before " + harness.SecretMarker(ref) + " after")
		got := harness.SecretRefs(content)
		if valid := domain.ValidSecretRef(ref); valid != slices.Equal(got, []string{ref}) || !valid && len(got) != 0 {
			t.Errorf("ref %q (valid %v): SecretRefs = %q", ref, valid, got)
		}
		err := harness.File{Path: "f", Content: content, SecretRefs: got}.CheckSecrets()
		if domain.ValidSecretRef(ref) != (err == nil) {
			t.Errorf("ref %q: CheckSecrets = %v", ref, err)
		}
	}
}

func TestAMarkerInsideAnotherIsNotTakenForValid(t *testing.T) {
	content := []byte("{{cf-secret:a" + harness.SecretMarker("b"))
	if got := harness.SecretRefs(content); !slices.Equal(got, []string{"b"}) {
		t.Errorf("SecretRefs = %q", got)
	}
	if err := (harness.File{Content: content, SecretRefs: []string{"b"}}).CheckSecrets(); !errors.Is(err, harness.ErrSecretUnavailable) {
		t.Errorf("CheckSecrets = %v, want ErrSecretUnavailable for the open marker", err)
	}
}
