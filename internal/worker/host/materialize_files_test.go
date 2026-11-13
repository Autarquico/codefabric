package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

func materializeWith(t *testing.T, opts ...MaterializerOption) (Materialization, string, harness.Host) {
	t.Helper()
	h, _ := testHost(t)
	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	mat, err := NewMaterializer(h, opts...).Materialize(context.Background(), newMini(), run, "", snapshot("A"))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return mat, run, h
}

func TestMaterialize_AddsTheFilesOfItsHarnessToTheConfiguration(t *testing.T) {
	skill := []byte("---\nname: handto\n---\n")
	mat, run, h := materializeWith(t, WithFiles("mini", map[string][]byte{"skills/handto/SKILL.md": skill}))

	path := filepath.Join(mat.ConfigDir, "skills", "handto", "SKILL.md")
	got, err := os.ReadFile(path) //nolint:gosec // a test path
	if err != nil || string(got) != string(skill) {
		t.Fatalf("the skill is %q, %v; want it in the configuration directory of the Run", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the skill is %v, %v; want 0600", info, err)
	}
	// What was added is part of what the configuration digests to, so the Run is reused as it is.
	if d, err := treedigest.Dir(mat.ConfigDir, treedigest.Options{ApprovedLinks: mat.AuthLinks}); err != nil || d != mat.Digest {
		t.Errorf("digest = %s, %v; recorded %s", d, err, mat.Digest)
	}
	again, err := NewMaterializer(h, WithFiles("mini", map[string][]byte{"skills/handto/SKILL.md": skill})).
		Materialize(context.Background(), newMini(), run, "", snapshot("A"))
	if err != nil || again.Digest != mat.Digest {
		t.Errorf("materialising again = %s, %v; want %s", again.Digest, err, mat.Digest)
	}
}

func TestMaterialize_FilesOfAnotherHarnessAreNotAdded(t *testing.T) {
	mat, _, _ := materializeWith(t, WithFiles("claude-code", map[string][]byte{"skills/handto/SKILL.md": []byte("x")}))
	if _, err := os.Stat(filepath.Join(mat.ConfigDir, "skills", "handto")); !os.IsNotExist(err) {
		t.Errorf("a file for claude-code is in the configuration of mini: %v", err)
	}
}

func TestMaterialize_WhatTheUserConfiguredWinsOverWhatCfAdds(t *testing.T) {
	mat, _, _ := materializeWith(t, WithFiles("mini", map[string][]byte{"skills/review/SKILL.md": []byte("cf's")}))
	got, err := os.ReadFile(filepath.Join(mat.ConfigDir, "skills", "review", "SKILL.md")) //nolint:gosec // a test path
	if err != nil || string(got) != "# review\n" {
		t.Errorf("the skill is %q, %v; want the one of the snapshot", got, err)
	}
}

func TestMaterialize_RefusesAFileOutsideTheConfiguration(t *testing.T) {
	h, _ := testHost(t)
	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "/abs", "a/../../b"} {
		_, err := NewMaterializer(h, WithFiles("mini", map[string][]byte{name: []byte("x")})).
			Materialize(context.Background(), newMini(), run, "", snapshot("A"))
		if err == nil {
			t.Errorf("%q: the file was accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(run), "escape")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the configuration: %v", err)
	}
}
