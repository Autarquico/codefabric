package executor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// A registry of rendered files that cannot be read makes the janitor of the
// secrets discard the configuration tree, but the auth copy in it holds the
// last refresh of the Run: it stays until the Broker has reconciled it.
func TestTheDiscardOfAnUnreadableRegistryKeepsTheAuthCopyUntilTheBrokerReconciledIt(t *testing.T) {
	rec := &authRecorder{tracked: map[string][]string{}, releaseErr: errors.New("host is read-only")}
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}))
	f.e.o.AuthSync = rec
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	dir := f.runDir(k)
	copied := materialised(t, dir)
	rec.tracked[authID(k)] = []string{copied}
	rendered := filepath.Join(dir, "config", "config.toml")
	err := f.e.jit.Render(dir, 1, []string{"config/config.toml"}, func() error { return nil }, func() error {
		return os.WriteFile(rendered, []byte(tomlValue), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rendered.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.e.forget(k) // the Broker cannot keep the refresh: the copies stay
	if err := f.e.sweepSecrets(t.Context()); err != nil {
		t.Fatalf("the release of the secrets: %v", err)
	}
	if !exists(copied) {
		t.Fatal("the discard removed the auth copy before the Broker reconciled it")
	}
	if exists(rendered) {
		t.Error("the rendered file was kept")
	}

	rec.releaseErr = nil
	f.e.sweep(t.Context())
	if exists(copied) {
		t.Error("the copy outlived the reconcile")
	}
}
