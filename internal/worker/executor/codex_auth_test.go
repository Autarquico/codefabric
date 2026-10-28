//go:build linux || darwin

package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/worker/authsync"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// codexAuth is an auth.json that Codex refreshed at the offset from t0.
func codexAuth(refreshed time.Duration) string {
	return `{"tokens":{"access_token":"token-` + refreshed.String() + `"},"last_refresh":"` +
		t0.Add(refreshed).Format(time.RFC3339Nano) + `"}`
}

func writeAuth(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAuth(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A Run of codex gets its own copy of the auth of the host (not a link), the
// Worker keeps it in step with the host's while the Run lives, and forgets it
// when the Run is over.
func TestARunOfCodexHasItsAuthCopyBrokeredWhileItLives(t *testing.T) {
	broker := authsync.New(authsync.Options{})
	f := newFixture(t, func(o *Options) {
		o.Host = hostWithCodex(t)
		o.Harnesses = registry(t, codex.Adapter{})
		o.AuthSync = broker
		o.ConfigSource = func(context.Context, state.Run, string) (harness.NativeSnapshot, error) {
			return harness.NativeSnapshot{Harness: codex.ID, SchemaVersion: codex.SchemaVersion}, nil
		}
	})
	global := codex.Adapter{}.GlobalAuth(f.opts.Host)
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	writeAuth(t, global, codexAuth(time.Minute))

	k := f.assign(specJSON("fix it", codex.ID, "headless"))
	if f.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}

	copied := filepath.Join(f.runDir(k), "config", codex.AuthFile)
	fi, err := os.Lstat(copied)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the auth of the Run: %v, %v; want a private regular file", fi, err)
	}
	if got := readAuth(t, copied); got != codexAuth(time.Minute) {
		t.Errorf("the copy is %s, want the auth of the host", got)
	}

	// Codex refreshes the token in its copy: the Worker brings it to the host.
	writeAuth(t, copied, codexAuth(time.Hour))
	broker.Poll()
	if got := readAuth(t, global); got != codexAuth(time.Hour) {
		t.Errorf("the host has %s after a refresh of the live Run", got)
	}

	// The Run ends: nothing of it is kept, so a later write to its copy goes nowhere.
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(2 * time.Second), Cause: execwrap.CauseExit})
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run was not settled")
	}
	writeAuth(t, copied, codexAuth(2*time.Hour))
	broker.Poll()
	if got := readAuth(t, global); got != codexAuth(time.Hour) {
		t.Errorf("the host has %s: the Run, over, is still brokered", got)
	}
}

// hostWithCodex is a Host with a codex binary of the test in its PATH: Start
// names the binary by its path.
func hostWithCodex(t *testing.T) harness.Host {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + bin + ":/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
