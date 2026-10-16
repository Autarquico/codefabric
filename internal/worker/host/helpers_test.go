package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// credentialSentinel is the content of the credentials of the test HOME: it
// must never be copied into a configuration directory.
const credentialSentinel = "SENTINEL-credential-7f3a" //nolint:gosec // a test sentinel, not a credential

// mini is the least adapter with ConfigIsolation: it writes the files of a
// snapshot into a TargetRun and links ~/.mini/credentials.json beside them.
type mini struct {
	harness.Unsupported
	id   string
	caps harness.Capabilities

	applies atomic.Int32
	// applyErr makes ApplyConfig fail after it wrote part of the target.
	applyErr error
	// strayLink makes ApplyConfig leave a symlink it does not report.
	strayLink bool
	// detect replaces Detect, which otherwise finds it installed.
	detect func(ctx context.Context) (harness.Detection, error)
}

func newMini() *mini {
	return &mini{caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}, ConfigIsolation: true}}
}

func (m *mini) ID() string {
	if m.id == "" {
		return "mini"
	}
	return m.id
}

func (m *mini) Capabilities() harness.Capabilities { return m.caps }

func (m *mini) Detect(ctx context.Context, _ harness.Host) (harness.Detection, error) {
	if m.detect != nil {
		return m.detect(ctx)
	}
	return harness.Detection{Installed: true, Path: "/opt/bin/" + m.ID()}, nil
}

func (m *mini) ApplyConfig(_ context.Context, h harness.Host, snap harness.NativeSnapshot, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	m.applies.Add(1)
	run, ok := opts.Target.(harness.TargetRun)
	if !ok {
		return harness.ApplyResult{}, harness.NewUnsupportedError("apply config to the host")
	}
	for _, c := range snap.Components {
		for _, f := range c.Files {
			p := filepath.Join(run.Dir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return harness.ApplyResult{}, err
			}
			if err := os.WriteFile(p, f.Content, f.Mode); err != nil {
				return harness.ApplyResult{}, err
			}
			if m.applyErr != nil {
				return harness.ApplyResult{}, m.applyErr
			}
		}
	}
	var res harness.ApplyResult
	if opts.LinkAuth {
		if err := os.Symlink(filepath.Join(h.Home(), ".mini", "credentials.json"), filepath.Join(run.Dir, "credentials.json")); err != nil {
			return harness.ApplyResult{}, err
		}
		res.AuthLinks = []string{"credentials.json"}
	}
	if m.strayLink {
		if err := os.Symlink(filepath.Join(h.Home(), ".mini", "settings.json"), filepath.Join(run.Dir, "stray.json")); err != nil {
			return harness.ApplyResult{}, err
		}
	}
	return res, nil
}

// snapshot is a configuration of mini whose settings say settings.
func snapshot(settings string) harness.NativeSnapshot {
	return harness.NativeSnapshot{Harness: "mini", SchemaVersion: 1, Components: []harness.Component{
		{Name: "settings", Kind: harness.KindFile, Ownership: harness.Managed, Files: []harness.File{
			{Path: "settings.json", Mode: 0o600, Content: []byte(`{"theme":"` + settings + `"}`)},
		}},
		{Name: "skills", Kind: harness.KindDir, Ownership: harness.Managed, Files: []harness.File{
			{Path: "skills/review/SKILL.md", Mode: 0o600, Content: []byte("# review\n")},
			{Path: "skills/run.sh", Mode: 0o700, Content: []byte("#!/bin/sh\n")},
		}},
	}}
}

// testHost is a localhost.Host whose HOME holds the native configuration of
// mini: its settings, its credentials and a symlink.
func testHost(t *testing.T) (*localhost.Host, *clock.Fake) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".mini")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "settings.json"), `{"theme":"home"}`, 0o640)
	writeFile(t, filepath.Join(dir, "credentials.json"), credentialSentinel, 0o600)
	if err := os.Symlink("settings.json", filepath.Join(dir, "current.json")); err != nil {
		t.Fatal(err)
	}
	fake := clock.NewFake(time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC))
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=/usr/bin:/bin"}, Clock: fake})
	if err != nil {
		t.Fatal(err)
	}
	return h, fake
}

func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// manifest describes the tree under root by itself, without treedigest:
// every entry with its full mode, the content of each file and the target
// of each symlink.
func manifest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = info.Mode().String() + " -> " + target
		case info.IsDir():
			out[rel] = info.Mode().String()
		default:
			data, err := os.ReadFile(p) //nolint:gosec // a test tree
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = info.Mode().String() + " " + hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameManifest(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %s was %q, is %q", what, k, v, after[k])
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s: %s appeared: %q", what, k, v)
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// noRegularFileContains fails if a regular file under root holds s.
func noRegularFileContains(t *testing.T, root, s string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // a test tree
		if err != nil {
			return err
		}
		if strings.Contains(string(data), s) {
			t.Errorf("%s holds %q", p, s)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
