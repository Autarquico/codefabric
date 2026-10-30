package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
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

// Under deny the Worker never looks at project.yaml, so neither does explain:
// a link or a directory there is no error, and the file is only reported present.
func TestReadRepoFileUnderDenyIgnoresWhatIsThere(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string){
		"symlink": func(t *testing.T, dir string) {
			if err := os.Symlink("elsewhere", filepath.Join(dir, repoconfig.Path)); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, repoconfig.Path, "x"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, repoconfig.Path, "x", "f"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			gitIn(t, dir, "init", "-q")
			if err := os.MkdirAll(filepath.Join(dir, ".codefabric"), 0o750); err != nil {
				t.Fatal(err)
			}
			setup(t, dir)
			gitIn(t, dir, "add", "-A")
			gitIn(t, dir, "commit", "-q", "-m", "c")
			commit := gitIn(t, dir, "rev-parse", "HEAD")

			var denied v1.ExplainProjectRequest
			if err := readRepoFile(t.Context(), dir, commit, domain.RepoConfigDeny, &denied); err != nil {
				t.Fatalf("under deny: %v", err)
			}
			if !denied.FilePresent || denied.File != nil {
				t.Errorf("under deny: %+v, want present and unread", denied)
			}
			var allowed v1.ExplainProjectRequest
			if err := readRepoFile(t.Context(), dir, commit, domain.RepoConfigAllow, &allowed); err == nil {
				t.Error("under allow a non-regular project.yaml must be refused")
			}
		})
	}
}

// A name that starts with '-' is a name, as for the Worker's gitops.BlobIDs;
// and pathspec magic is not magic.
func TestBlobIDTakesDashNamesAndLiteralPathspecs(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	for _, name := range []string{"-lock", "a.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "c")
	commit := gitIn(t, dir, "rev-parse", "HEAD")

	want := gitIn(t, dir, "rev-parse", commit+":-lock")
	if got, err := blobID(t.Context(), dir, commit, "-lock"); err != nil || got != want {
		t.Errorf("blobID(-lock) = %q, %v; want %q", got, err, want)
	}
	for _, magic := range []string{":(glob)*.txt", "*.txt"} {
		if got, err := blobID(t.Context(), dir, commit, magic); err != nil || got != "" {
			t.Errorf("blobID(%q) = %q, %v; want nothing: pathspecs are literal", magic, got, err)
		}
	}
}
