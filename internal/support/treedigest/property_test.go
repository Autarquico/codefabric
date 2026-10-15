package treedigest

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

const (
	// envSeed replays one seed; without it each run draws a new one.
	envSeed = "CF_TREEDIGEST_SEED"
	// envTrees sets how many trees a property tries; 100 by default.
	envTrees = "CF_TREEDIGEST_TREES"
)

// nameRunes has no "." (so no "." or ".." names), no "~" (renames append it
// to stay unique) and no letters with a case pair or two normalizations, so
// the trees fit on case- and normalization-insensitive file systems.
var nameRunes = []rune("abc-_1ñ€")

var time0 = time.Unix(0, 0)

func TestProperty_TreeDigest_OrderDoesNotMatter(t *testing.T) {
	forEachTree(t, func(t *testing.T, r *rand.Rand, entries []Entry) {
		shuffled := slices.Clone(entries)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if a, b := mustFiles(t, entries), mustFiles(t, shuffled); a != b {
			t.Fatalf("shuffling changed the digest: %s != %s\n%v\n%v", a, b, entries, shuffled)
		}
	})
}

func TestProperty_TreeDigest_ChangesAreSeen(t *testing.T) {
	mutations := map[string]func(File, *rand.Rand) File{
		"content byte": func(f File, r *rand.Rand) File {
			if len(f.Content) == 0 {
				f.Content = []byte{byte(r.Uint32())} //nolint:gosec // any byte will do
				return f
			}
			f.Content = slices.Clone(f.Content)
			f.Content[r.IntN(len(f.Content))] ^= byte(1 + r.IntN(255)) //nolint:gosec // 1..255
			return f
		},
		"exec bit": func(f File, _ *rand.Rand) File { f.Exec = !f.Exec; return f },
		"path":     func(f File, _ *rand.Rand) File { f.Path += "~"; return f },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			forEachTree(t, func(t *testing.T, r *rand.Rand, entries []Entry) {
				files := fileIndexes(entries)
				i := files[r.IntN(len(files))]
				changed := slices.Clone(entries)
				changed[i] = mutate(entries[i].(File), r)
				if a, b := mustFiles(t, entries), mustFiles(t, changed); a == b {
					t.Fatalf("changing %v to %v kept the digest %s", entries[i], changed[i], a)
				}
			})
		})
	}
}

func TestProperty_TreeDigest_DirMatchesFiles(t *testing.T) {
	forEachTree(t, func(t *testing.T, _ *rand.Rand, entries []Entry) {
		root := t.TempDir()
		writeTree(t, root, entries)
		if a, b := mustFiles(t, entries), mustDir(t, root, Options{}); a != b {
			t.Fatalf("Files = %s, Dir = %s for %v", a, b, entries)
		}
	})
}

// forEachTree runs check on random trees, each with at least one file, drawn
// from a seed it logs so that a failure can be replayed.
func forEachTree(t *testing.T, check func(*testing.T, *rand.Rand, []Entry)) {
	t.Helper()
	seed := rand.Uint64() //nolint:gosec // a seed to repeat, not a secret
	if text := os.Getenv(envSeed); text != "" {
		var err error
		if seed, err = strconv.ParseUint(text, 0, 64); err != nil {
			t.Fatalf("%s=%q is not a seed", envSeed, text)
		}
	}
	trees := 100
	if text := os.Getenv(envTrees); text != "" {
		var err error
		if trees, err = strconv.Atoi(text); err != nil || trees < 1 {
			t.Fatalf("%s=%q is not a positive count", envTrees, text)
		}
	}
	t.Logf("seed %#x (replay with %s=%#x)", seed, envSeed, seed)
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // a seed to repeat, not a secret
	for n := range trees {
		entries := randomTree(r)
		t.Run(strconv.Itoa(n), func(t *testing.T) { check(t, r, entries) })
		if t.Failed() {
			return
		}
	}
}

func randomTree(r *rand.Rand) []Entry {
	for {
		entries := randomDir(r, "", 0)
		if len(fileIndexes(entries)) > 0 {
			return entries
		}
	}
}

// randomDir returns the entries under prefix: up to five children, each a
// file or, above depth three, a directory, listed when empty and sometimes
// when not.
func randomDir(r *rand.Rand, prefix string, depth int) []Entry {
	var entries []Entry
	seen := map[string]bool{}
	for range r.IntN(6) {
		name := randomName(r)
		if seen[name] {
			continue
		}
		seen[name] = true
		p := prefix + name
		if depth < 3 && r.IntN(3) == 0 {
			children := randomDir(r, p+"/", depth+1)
			if len(children) == 0 || r.IntN(4) == 0 {
				entries = append(entries, Directory{Path: p})
			}
			entries = append(entries, children...)
			continue
		}
		content := make([]byte, r.IntN(65))
		for i := range content {
			content[i] = byte(r.Uint32()) //nolint:gosec // any byte will do
		}
		entries = append(entries, File{Path: p, Exec: r.IntN(2) == 0, Content: content})
	}
	return entries
}

func randomName(r *rand.Rand) string {
	name := make([]rune, 1+r.IntN(3))
	for i := range name {
		name[i] = nameRunes[r.IntN(len(nameRunes))]
	}
	return string(name)
}

func fileIndexes(entries []Entry) []int {
	var files []int
	for i, e := range entries {
		if _, ok := e.(File); ok {
			files = append(files, i)
		}
	}
	return files
}

// writeTree creates the entries under root, files 0700 or 0600 by their bit.
func writeTree(t *testing.T, root string, entries []Entry) {
	t.Helper()
	for _, e := range entries {
		switch e := e.(type) {
		case Directory:
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(e.Path)), 0o750); err != nil {
				t.Fatal(err)
			}
		case File:
			p := filepath.Join(root, filepath.FromSlash(e.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			perm := os.FileMode(0o600)
			if e.Exec {
				perm = 0o700
			}
			if err := os.WriteFile(p, e.Content, perm); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, perm); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func mustFiles(t *testing.T, entries []Entry) string {
	t.Helper()
	d, err := Files(entries)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustDir(t *testing.T, root string, opts Options) string {
	t.Helper()
	d, err := Dir(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
