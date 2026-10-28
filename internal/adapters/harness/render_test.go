package harness

import (
	"errors"
	"testing"
)

func TestRenderSecretsReplacesEachMarkerOnceAndNeverExpandsAValue(t *testing.T) {
	content := []byte("a=" + SecretMarker("team/a") + " b=" + SecretMarker("team/b") + " a2=" + SecretMarker("team/a") + " {{cf-secret:../bad}}")
	got, err := RenderSecrets(content, func(ref string) ([]byte, error) {
		if ref == "team/a" {
			return []byte(SecretMarker("team/b")), nil // looks like a marker: not expanded again
		}
		return []byte("B"), nil
	})
	want := "a=" + SecretMarker("team/b") + " b=B a2=" + SecretMarker("team/b") + " {{cf-secret:../bad}}"
	if err != nil || string(got) != want {
		t.Errorf("got %q (%v), want %q", got, err, want)
	}
}

func TestRenderSecretsStopsAtTheFirstValueItCannotGive(t *testing.T) {
	boom := errors.New("no value")
	if _, err := RenderSecrets([]byte(SecretMarker("team/a")), func(string) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}
