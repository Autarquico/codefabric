package secretjit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

var (
	v1 = []redact.Needle{
		{Ref: "api/token", Value: []byte("old-token-value-1")},
		{Ref: "db.password", Value: []byte("old-password-value-1")},
	}
	v2 = []redact.Needle{
		{Ref: "api/token", Value: []byte("new-token-value-2")},
		{Ref: "db.password", Value: []byte("new-password-value-2\nsecond-line")},
	}
)

func allow() error { return nil }

// runDir is a fresh run directory, as the executor makes it.
func runDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs", "01JRUN-e1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func deliver(t *testing.T, dir string, attempt uint64, secrets []redact.Needle) {
	t.Helper()
	if err := New().Deliver(dir, attempt, secrets, allow); err != nil {
		t.Fatalf("Deliver(attempt %d): %v", attempt, err)
	}
}

func sameNeedles(a, b []redact.Needle) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Ref != b[i].Ref || !bytes.Equal(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}

// wantLoad fails unless the delivery in dir is want.
func wantLoad(t *testing.T, dir string, want []redact.Needle) {
	t.Helper()
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !sameNeedles(got, want) {
		t.Fatalf("Load = %q, want %q", got, want)
	}
}

// wantOnly fails unless, of the directories of a delivery, dir holds exactly
// names.
func wantOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range []string{liveName, stagingName, deletingName} {
		_, err := os.Lstat(filepath.Join(dir, name))
		want := false
		for _, n := range names {
			want = want || n == name
		}
		if got := err == nil; got != want {
			t.Errorf("%s present = %v, want %v (%v)", name, got, want, err)
		}
	}
}

// noValue fails if err says any of the values.
func noValue(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, set := range [][]redact.Needle{v1, v2} {
		for _, s := range set {
			for _, part := range strings.Split(string(s.Value), "\n") {
				if strings.Contains(err.Error(), part) {
					t.Fatalf("error says a value: %v", err)
				}
			}
		}
	}
}

var errInjected = errors.New("injected fault")

// crash is what an injected crash panics with: the process dies there, and
// nothing after it, not even an error path, runs.
type crash struct{}

// mode is how an injected fault strikes at its step.
type mode struct {
	name  string
	after bool // the step is done, and then it fails
	crash bool // it panics instead of returning an error
}

var modes = []mode{
	{name: "error before"},
	{name: "error after", after: true},
	{name: "crash before", crash: true},
	{name: "crash after", after: true, crash: true},
}

// faults wraps the real file system and strikes at step at (from 1), or
// never if at is 0. It records the steps it sees.
type faults struct {
	at    int
	mode  mode
	steps []string
}

func (f *faults) hit(step string, do func() error) error {
	f.steps = append(f.steps, step)
	if len(f.steps) != f.at {
		return do()
	}
	if f.mode.after {
		if err := do(); err != nil {
			return err
		}
	}
	if f.mode.crash {
		panic(crash{})
	}
	return errInjected
}

func (f *faults) manager() *Manager {
	osfs := osOps()
	return &Manager{fs: fsOps{
		mkdir: func(p string) error { return f.hit("mkdir "+filepath.Base(p), func() error { return osfs.mkdir(p) }) },
		writeFile: func(p string, data []byte) error {
			return f.hit("write "+stepName(p), func() error { return osfs.writeFile(p, data) })
		},
		sync: func(p string) error { return f.hit("sync "+stepName(p), func() error { return osfs.sync(p) }) },
		rename: func(a, b string) error {
			return f.hit("rename "+filepath.Base(a)+" "+filepath.Base(b), func() error { return osfs.rename(a, b) })
		},
		removeAll: func(p string) error {
			return f.hit("remove "+filepath.Base(p), func() error { return osfs.removeAll(p) })
		},
	}}
}

// stepName names a path by its base, and a value file as "value".
func stepName(p string) string {
	base := filepath.Base(p)
	if len(base) == 32 && !strings.Contains(base, ".") {
		return "value"
	}
	return base
}

// run calls op, recovering an injected crash, and reports whether it crashed.
func run(op func() error) (crashed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	return false, op()
}
