package codex_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
)

func TestAuthStamp(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 30, 0, 123, time.UTC)
	for _, tc := range []struct {
		name, content string
		want          time.Time
		identity      string
		ok            bool
	}{
		{"refreshed", `{"tokens":{"account_id":"acct-1"},"last_refresh":"2026-10-28T09:30:00.000000123Z"}`, at, "acct-1", true},
		{"no account", `{"tokens":{},"last_refresh":"2026-10-28T09:30:00.000000123Z"}`, at, "", true},
		{"never refreshed", `{"OPENAI_API_KEY":"k"}`, time.Time{}, "", true},
		{"null", `{"last_refresh":null,"tokens":null}`, time.Time{}, "", true},
		{"truncated", `{"tokens":{"access_token":"a`, time.Time{}, "", false},
		{"empty", ``, time.Time{}, "", false},
		{"not an object", `[1]`, time.Time{}, "", false},
		{"bad timestamp", `{"last_refresh":"yesterday"}`, time.Time{}, "", false},
	} {
		got, who, err := codex.Adapter{}.AuthStamp([]byte(tc.content))
		if (err == nil) != tc.ok || !got.Equal(tc.want) || who != tc.identity {
			t.Errorf("%s: AuthStamp = %v, %q, %v", tc.name, got, who, err)
		}
	}
}

func TestGlobalAuth(t *testing.T) {
	h := homeWith(t, nil)
	if got := (codex.Adapter{}).GlobalAuth(h); got != filepath.Join(codexDir(h), "auth.json") {
		t.Errorf("GlobalAuth = %s", got)
	}
}

func TestApplyRun_NoAuthInTheHomeCopiesNothing(t *testing.T) {
	h := homeWith(t, nil)
	if err := os.Remove(filepath.Join(codexDir(h), "auth.json")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := codex.Adapter{}.ApplyConfig(t.Context(), h, snapshot(t, h), harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true})
	if err != nil || len(res.AuthCopies) != 0 || len(res.AuthLinks) != 0 {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "auth.json")); err == nil {
		t.Error("an auth.json appeared with none in the home")
	}
}

func TestApplyGlobal_LeavesTheAuthAlone(t *testing.T) {
	h := homeWith(t, nil)
	path := filepath.Join(codexDir(h), "auth.json")
	before, _ := os.ReadFile(path) //nolint:gosec // temporary dir
	res, err := codex.Adapter{}.ApplyConfig(t.Context(), h, snapshotWithoutSecrets(t, h), harness.ApplyOptions{Target: harness.TargetGlobal{}, LinkAuth: true})
	if err != nil || len(res.AuthCopies) != 0 || len(res.AuthLinks) != 0 {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) { //nolint:gosec // temporary dir
		t.Error("a global apply changed auth.json")
	}
}

// snapshotWithoutSecrets is a snapshot a global apply accepts: no markers.
func snapshotWithoutSecrets(t *testing.T, h harness.Host) harness.NativeSnapshot {
	t.Helper()
	return snapshot(t, h, "AGENTS.md")
}
