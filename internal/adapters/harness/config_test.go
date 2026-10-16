package harness_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

func TestFilesDigestIsTheDigestOfTheTreeTheFilesMake(t *testing.T) {
	files := []harness.File{
		{Path: "settings.json", Mode: 0o600, Content: []byte(`{"model":"m1"}`)},
		{Path: "bin/hook.sh", Mode: 0o700, Content: []byte("#!/bin/sh\n")},
		{Path: "skills/a/SKILL.md", Mode: 0o644, Content: []byte("a")},
	}
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Content, f.Mode); err != nil {
			t.Fatal(err)
		}
	}
	got, err := harness.FilesDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	if want, err := treedigest.Dir(root, treedigest.Options{}); err != nil || got != want {
		t.Errorf("FilesDigest = %s; the tree on disk is %s, %v", got, want, err)
	}
}

func TestFilesDigestSeesOnlyTheExecBitOfTheOwner(t *testing.T) {
	digest := func(mode fs.FileMode) string {
		d, err := harness.FilesDigest([]harness.File{{Path: "hook.sh", Mode: mode, Content: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if digest(0o600) != digest(0o644) || digest(0o600) != digest(0o611) {
		t.Error("the digest changes with permissions other than the exec bit of the owner")
	}
	if digest(0o700) != digest(0o755) {
		t.Error("two executable files have different digests")
	}
	if digest(0o600) == digest(0o700) {
		t.Error("an executable file has the digest of a plain one")
	}
}

func TestFilesDigestRefusesWhatIsNotATree(t *testing.T) {
	if _, err := harness.FilesDigest([]harness.File{{Path: "../escape", Content: []byte("x")}}); err == nil {
		t.Error("FilesDigest took a path out of the root")
	}
}
