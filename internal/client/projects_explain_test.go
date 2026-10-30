package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/repoconfig"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // test fixture: fixed git arguments
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func cloneWithConfig(t *testing.T) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q")
	for path, body := range map[string]string{
		repoconfig.Path:       "version: 1\n",
		"migrations/001.sql":  "select 1;\n",
		"deep/er/placeholder": "x\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "c")
	return dir, gitIn(t, dir, "rev-parse", "HEAD")
}

// The paths are the repository's, whatever directory of the clone the command runs in.
func TestTreeEntryFromSubdirectory(t *testing.T) {
	dir, commit := cloneWithConfig(t)
	sub := filepath.Join(dir, "deep", "er")
	want := gitIn(t, dir, "rev-parse", commit+":"+repoconfig.Path)
	got, err := fileBlob(context.Background(), sub, commit, repoconfig.Path)
	if err != nil || got != want {
		t.Fatalf("fileBlob from a subdirectory = %q, %v; want %q", got, err, want)
	}
}

// A digest input is whatever object the commit has at the path, as for the Worker.
func TestBlobIDOfDirectoryIsItsTreeID(t *testing.T) {
	dir, commit := cloneWithConfig(t)
	ctx := context.Background()
	want := gitIn(t, dir, "rev-parse", commit+":migrations")
	got, err := blobID(ctx, filepath.Join(dir, "deep"), commit, "migrations")
	if err != nil || got != want {
		t.Fatalf("blobID(dir) = %q, %v; want %q", got, err, want)
	}
	if got, err := blobID(ctx, dir, commit, "missing"); err != nil || got != "" {
		t.Fatalf("blobID(missing) = %q, %v; want empty", got, err)
	}
	if _, err := fileBlob(ctx, dir, commit, "migrations"); err == nil {
		t.Fatal("a directory is not a project.yaml")
	}
}
