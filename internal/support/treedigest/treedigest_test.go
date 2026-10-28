package treedigest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The vectors were computed by an independent implementation of the format
// in the package doc, so they pin the encoding as well as the code.
const (
	digestEmpty    = "sha256:400367e38fbbb295304dcfdef91d238ad6153c51bdc3e0ca1f2081e8acac0273"
	digestHello    = "sha256:ced27622cb31a258dbb2e49b74539b273c5d9c9c0cda789a55d87a9e5a21a508"
	digestHelloX   = "sha256:271201899fdd78564dbcb59631ad2877f3c0c3a454930f0b7484d8222fcaa238"
	digestEmptyDir = "sha256:3beaaf9bb45c00500453de33fd35e9d5fd095fdc6691ff2a31927a70d7a8504b"
	digestNested   = "sha256:6f725078a38124df5e45dd817b82cceec67d5ecfe2ac2893fc0c518269606f35"
	digestNFC      = "sha256:4ecc6d00c627dafcd00f34e6b02531cef74311e638e02295215bb60e21251882"
	digestNFD      = "sha256:05c3db41fd3cf8d51633e281d69abe300d5b2aa2be7bc343232af0f41de7a21b"
)

func TestVectors(t *testing.T) {
	cases := []struct {
		name    string
		entries []Entry
		want    string
	}{
		{"empty tree", nil, digestEmpty},
		{"one file", []Entry{File{Path: "hello.txt", Content: []byte("hello\n")}}, digestHello},
		{"exec bit", []Entry{File{Path: "hello.txt", Exec: true, Content: []byte("hello\n")}}, digestHelloX},
		{"empty directory", []Entry{Directory{Path: "empty"}}, digestEmptyDir},
		{"implied directories", []Entry{
			File{Path: "z"},
			File{Path: "a/b/c", Content: []byte("x")},
		}, digestNested},
		{"listed ancestors change nothing", []Entry{
			Directory{Path: "a"},
			File{Path: "z"},
			Directory{Path: "a/b"},
			File{Path: "a/b/c", Content: []byte("x")},
		}, digestNested},
		{"precomposed name", []Entry{File{Path: "é"}}, digestNFC},
		{"decomposed name", []Entry{File{Path: "é"}}, digestNFD},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Files(c.entries)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("Files = %s, want %s", got, c.want)
			}
			root := t.TempDir()
			writeTree(t, root, c.entries)
			if got := mustDir(t, root, Options{}); got != c.want {
				t.Fatalf("Dir = %s, want %s", got, c.want)
			}
		})
	}
}

func TestFiles_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		entries []Entry
		want    error
	}{
		{"empty path", []Entry{File{}}, ErrInvalidPath},
		{"absolute", []Entry{File{Path: "/etc/passwd"}}, ErrInvalidPath},
		{"dot dot", []Entry{File{Path: "a/../b"}}, ErrInvalidPath},
		{"leading dot dot", []Entry{Directory{Path: "../a"}}, ErrInvalidPath},
		{"dot", []Entry{File{Path: "./a"}}, ErrInvalidPath},
		{"empty component", []Entry{File{Path: "a//b"}}, ErrInvalidPath},
		{"trailing slash", []Entry{Directory{Path: "a/"}}, ErrInvalidPath},
		{"invalid UTF-8", []Entry{File{Path: "a\xffb"}}, ErrInvalidPath},
		{"NUL", []Entry{File{Path: "a\x00b"}}, ErrInvalidPath},
		{"duplicate file", []Entry{File{Path: "a"}, File{Path: "a", Content: []byte("x")}}, ErrDuplicatePath},
		{"duplicate directory", []Entry{Directory{Path: "a"}, Directory{Path: "a"}}, ErrDuplicatePath},
		{"file and directory", []Entry{File{Path: "a"}, Directory{Path: "a"}}, ErrDuplicatePath},
		{"file as ancestor", []Entry{File{Path: "a/b"}, File{Path: "a"}}, ErrCollision},
		{"file as deep ancestor", []Entry{File{Path: "a"}, Directory{Path: "a/b/c"}}, ErrCollision},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Files(c.entries)
			if !errors.Is(err, c.want) {
				t.Fatalf("Files = %q, %v; want %v", got, err, c.want)
			}
		})
	}
	if _, err := Files([]Entry{nil}); err == nil {
		t.Fatal("Files accepted a nil entry")
	}
}

func TestDir_IgnoresTimesAndOtherPermissions(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []Entry{File{Path: "hello.txt", Content: []byte("hello\n")}})
	p := filepath.Join(root, "hello.txt")
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time0, time0); err != nil {
		t.Fatal(err)
	}
	if got := mustDir(t, root, Options{}); got != digestHello {
		t.Fatalf("Dir = %s, want %s", got, digestHello)
	}
	if err := os.Chmod(p, 0o500); err != nil { //nolint:gosec // the owner exec bit is under test
		t.Fatal(err)
	}
	if got := mustDir(t, root, Options{}); got != digestHelloX {
		t.Fatalf("Dir with owner exec = %s, want %s", got, digestHelloX)
	}
}

func TestDir_ApprovedLinkIsLeftOut(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []Entry{File{Path: "hello.txt", Content: []byte("hello\n")}, Directory{Path: "auth"}})
	secret := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(secret, []byte(`{"token":"s3cr3t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "auth", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	want, err := Files([]Entry{File{Path: "hello.txt", Content: []byte("hello\n")}, Directory{Path: "auth"}})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{ApprovedLinks: []string{"auth/credentials.json", "missing/link"}}
	if got := mustDir(t, root, opts); got != want {
		t.Fatalf("Dir = %s, want %s", got, want)
	}
	if err := os.WriteFile(secret, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustDir(t, root, opts); got != want {
		t.Fatalf("Dir after rotating the credential = %s, want %s", got, want)
	}
}

func TestDir_RejectsUnapprovedLink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("elsewhere", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if got, err := Dir(root, Options{ApprovedLinks: []string{"other"}}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("Dir = %q, %v; want %v", got, err, ErrUnsupportedType)
	}
}

func TestDir_RejectsApprovedPathThatIsNotALink(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []Entry{File{Path: "credentials.json", Content: []byte("copied")}})
	if got, err := Dir(root, Options{ApprovedLinks: []string{"credentials.json"}}); err == nil {
		t.Fatalf("Dir = %q, want an error", got)
	}
}

func TestDir_RejectsInvalidApprovedLink(t *testing.T) {
	if got, err := Dir(t.TempDir(), Options{ApprovedLinks: []string{"../x"}}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Dir = %q, %v; want %v", got, err, ErrInvalidPath)
	}
}

func TestDir_MissingRoot(t *testing.T) {
	if got, err := Dir(filepath.Join(t.TempDir(), "missing"), Options{}); err == nil {
		t.Fatalf("Dir = %q, want an error", got)
	}
}

func TestDir_ApprovedFilesAreLeftOut(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	without, err := Dir(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{ApprovedFiles: []string{"auth.json", "missing.json"}}
	one, err := Dir(root, opts)
	if err != nil || one != without {
		t.Fatalf("approved file digest = %s, %v; want %s", one, err, without)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if two, err := Dir(root, opts); err != nil || two != without {
		t.Errorf("the content of an approved file changed the digest: %s, %v", two, err)
	}
	if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := Dir(root, opts); err == nil {
		t.Errorf("an approved path that is a directory was accepted: %s", got)
	}
}
