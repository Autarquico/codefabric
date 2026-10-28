package secretjit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const renderedValue = "rendered-sentinel-value"

// render registers config/c.toml and writes a value to it, and to its
// temporary if temp is set (as a render that died mid-file leaves it).
func render(t *testing.T, dir string, attempt uint64, temp bool) {
	t.Helper()
	err := New().Render(dir, attempt, []string{"config/c.toml"}, allow, func() error {
		if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
			return err
		}
		if temp {
			if err := os.WriteFile(filepath.Join(dir, RenderTemp("config/c.toml")), []byte(renderedValue), 0o600); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(dir, "config", "c.toml"), []byte(renderedValue), 0o600)
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestRenderRegistersBeforeItWritesAndUnderTheFence(t *testing.T) {
	dir := runDir(t)
	var seen bool
	err := New().Render(dir, 1, []string{"config/c.toml"}, allow, func() error {
		seen = HasRendered(dir) && !fileExists(filepath.Join(dir, "config", "c.toml"))
		return nil
	})
	if err != nil || !seen {
		t.Fatalf("Render = %v, registered before write = %v", err, seen)
	}
	fenced := errors.New("the Run moved on")
	wrote := false
	err = New().Render(dir, 2, []string{"config/c.toml"}, func() error { return fenced }, func() error { wrote = true; return nil })
	if !errors.Is(err, fenced) || wrote {
		t.Errorf("Render = %v, wrote = %v", err, wrote)
	}
}

func TestRenderRefusesAnOlderAttemptAndPathsOutsideTheRunDirectory(t *testing.T) {
	dir := runDir(t)
	render(t, dir, 2, false)
	if err := New().Render(dir, 1, []string{"config/c.toml"}, allow, func() error { return nil }); !errors.Is(err, ErrStale) {
		t.Errorf("an older attempt: %v", err)
	}
	for _, p := range []string{"", "../x", "/etc/passwd", "a/../b", "a//b", "."} {
		if err := New().Render(dir, 3, []string{p}, allow, func() error { return nil }); err == nil {
			t.Errorf("%q was registered", p)
		}
	}
}

func TestAFailedRenderRemovesWhatItWrote(t *testing.T) {
	dir := runDir(t)
	boom := errors.New("boom")
	err := New().Render(dir, 1, []string{"config/c.toml"}, allow, func() error {
		_ = os.MkdirAll(filepath.Join(dir, "config"), 0o700)
		_ = os.WriteFile(filepath.Join(dir, "config", "c.toml"), []byte(renderedValue), 0o600)
		_ = os.WriteFile(filepath.Join(dir, RenderTemp("config/c.toml")), []byte(renderedValue), 0o600)
		return boom
	})
	if !errors.Is(err, boom) || fileExists(filepath.Join(dir, "config", "c.toml")) || fileExists(filepath.Join(dir, RenderTemp("config/c.toml"))) {
		t.Errorf("err = %v, files left", err)
	}
}

func TestReleaseRemovesTheRenderedFilesWhenTheRunMayLetThemGo(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	render(t, dir, 1, true)
	hold := errors.New("still needed")
	if err := New().Release(dir, func() error { return hold }); !errors.Is(err, hold) {
		t.Fatalf("Release = %v", err)
	}
	if !fileExists(filepath.Join(dir, "config", "c.toml")) {
		t.Error("a release the Run is not ready for removed the file")
	}
	if err := New().Release(dir, allow); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config/c.toml", RenderTemp("config/c.toml"), "secrets"} {
		if fileExists(filepath.Join(dir, p)) {
			t.Errorf("%s is left", p)
		}
	}
	if !HasRendered(dir) {
		t.Error("the Run forgot that it used secrets")
	}
	if err := New().Release(dir, allow); err != nil {
		t.Errorf("a second release: %v", err)
	}
}

// Without a delivery, rendered files alone are released too.
func TestReleaseOfRenderedFilesAloneAndTheirJanitor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	dir := filepath.Join(root, "01JRUN-e1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	render(t, dir, 1, true)
	if err := New().Sweep(root, func(string, uint64) (bool, error) { return false, nil }, func(string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(dir, "config", "c.toml")) {
		t.Fatal("the janitor released a Run that is not done")
	}
	if err := New().Sweep(root, func(string, uint64) (bool, error) { return true, nil }, func(string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(dir, "config", "c.toml")) || fileExists(filepath.Join(dir, RenderTemp("config/c.toml"))) {
		t.Error("the janitor left the rendered files")
	}
}

func TestAnUnreadableRegistryDoesNotKeepTheDeliveryButStaysPending(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	if err := os.WriteFile(filepath.Join(dir, renderedName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New().Release(dir, allow); !errors.Is(err, ErrPending) {
		t.Errorf("Release = %v, want it pending", err)
	}
	if fileExists(filepath.Join(dir, "secrets")) {
		t.Error("the delivery was kept")
	}
}

func TestRemovingRenderedFilesDoesNotFollowASymlinkOutOfTheRunDirectory(t *testing.T) {
	dir := runDir(t)
	render(t, dir, 1, false)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "c.toml")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The harness swapped the directory for a link out of the run directory.
	if err := os.RemoveAll(filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	_ = New().Release(dir, allow)
	if !fileExists(victim) {
		t.Error("a file outside the run directory was removed")
	}
}
