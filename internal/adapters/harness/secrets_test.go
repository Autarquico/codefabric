package harness_test

import (
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
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
