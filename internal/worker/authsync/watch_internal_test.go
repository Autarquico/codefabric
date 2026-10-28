package authsync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

var t0 = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

// stamped is an AuthSyncer for {"at": <RFC3339>, "acct": <account>}.
type stamped struct{ global string }

func (a stamped) GlobalAuth(harness.Host) string { return a.global }

func (stamped) AuthStamp(content []byte) (time.Time, string, error) {
	var v struct {
		At   time.Time `json:"at"`
		Acct string    `json:"acct"`
	}
	if err := json.Unmarshal(content, &v); err != nil {
		return time.Time{}, "", errors.New("invalid")
	}
	return v.At, v.Acct, nil
}

func authAt(at time.Duration, pad string) []byte {
	b, _ := json.Marshal(map[string]any{"at": t0.Add(at), "acct": "A", "pad": pad})
	return b
}

type watchRig struct {
	b                    *Broker
	global, copyA, copyB string
}

func newWatchRig(t *testing.T) *watchRig {
	t.Helper()
	dir := t.TempDir()
	h, err := localhost.New(localhost.Options{Home: dir, Env: []string{"PATH=" + dir}})
	if err != nil {
		t.Fatal(err)
	}
	r := &watchRig{global: filepath.Join(dir, "global.json"), copyA: filepath.Join(dir, "a.json"), copyB: filepath.Join(dir, "b.json")}
	write := func(p string, c []byte) {
		if err := os.WriteFile(p, c, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(r.global, authAt(2*time.Minute, "g"))
	write(r.copyA, authAt(time.Minute, "a"))
	write(r.copyB, authAt(time.Minute, "b"))
	r.b = New(Options{Clock: clock.NewFake(t0.Add(time.Hour))})
	a := stamped{global: r.global}
	r.b.Track("a", a, h, []string{r.copyA})
	r.b.Track("b", a, h, []string{r.copyB})
	return r
}

func contentOf(t *testing.T, p string) string {
	t.Helper()
	c, err := os.ReadFile(p) //nolint:gosec // temporary dir
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

// Codex opens the copy with O_TRUNC after the Broker read it and before it
// renames over it, and writes its refresh once the rename is done: the write
// lands in the inode that no path names.
func TestReplace_KeepsARefreshWrittenIntoTheReplacedInode(t *testing.T) {
	r := newWatchRig(t)
	var codex *os.File
	r.b.hook = func(stage, path string) {
		if stage != "publish" || path != r.copyA {
			return
		}
		f, err := os.OpenFile(r.copyA, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // temporary dir
		if err != nil {
			t.Error(err)
			return
		}
		codex = f
	}
	r.b.Poll()
	if codex == nil {
		t.Fatal("the copy was never replaced")
	}
	newest := authAt(10*time.Minute, "codex")
	if _, err := codex.Write(newest); err != nil {
		t.Fatal(err)
	}
	if err := codex.Close(); err != nil {
		t.Fatal(err)
	}
	if got := contentOf(t, r.copyA); got != string(authAt(2*time.Minute, "g")) {
		t.Fatalf("precondition: the refresh must have missed the path, copy holds %q", got)
	}

	r.b.Poll()
	for name, p := range map[string]string{"global": r.global, "copy a": r.copyA, "copy b": r.copyB} {
		if got := contentOf(t, p); got != string(newest) {
			t.Errorf("%s = %q, want the refresh Codex wrote into the replaced inode", name, got)
		}
	}
}

// A copy truncated between the Broker reading it and publishing is not
// replaced: its writer is mid-refresh.
func TestReplace_SkipsACopyThatChangedSinceItWasRead(t *testing.T) {
	r := newWatchRig(t)
	r.b.hook = func(stage, path string) {
		if stage == "read" && path == r.copyA {
			if err := os.WriteFile(r.copyA, []byte(`{"partial`), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	r.b.Poll()
	if got := contentOf(t, r.copyA); got != `{"partial` {
		t.Errorf("copy a = %q, want it left alone", got)
	}
}

func TestWatches_AreBoundedAndClosed(t *testing.T) {
	r := newWatchRig(t)
	r.b.Poll()
	if n := len(r.b.watches); n != 2 {
		t.Fatalf("watches after replacing = %d, want 2", n)
	}
	for range watchPolls + 1 {
		r.b.Poll()
	}
	if n := len(r.b.watches); n != 0 {
		t.Errorf("watches after %d quiet polls = %d, want 0", watchPolls+1, n)
	}
	r.b.Track("c", stamped{global: r.global}, nil, []string{r.copyA})
	r.b.Poll() // nothing to replace, nothing to watch
	r.b.Untrack("c")
	if n := len(r.b.watches); n != 0 {
		t.Errorf("watches after untrack = %d, want 0", n)
	}
}

// hostOlderRig is the rig where the most recent version is a copy, so that the
// host is replaced too.
func hostOlderRig(t *testing.T) *watchRig {
	t.Helper()
	r := newWatchRig(t)
	for p, c := range map[string][]byte{r.global: authAt(time.Minute, "g"), r.copyA: authAt(5*time.Minute, "a")} {
		if err := os.WriteFile(p, c, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func closed(f *os.File) bool {
	_, err := f.Stat()
	return err != nil
}

func TestWatches_OfTheHostExpireAndCloseLikeThoseOfCopies(t *testing.T) {
	r := hostOlderRig(t)
	global, err := filepath.EvalSymlinks(r.global)
	if err != nil {
		t.Fatal(err)
	}
	r.b.Poll()
	host := r.b.watches[global]
	if host == nil || r.b.watches[r.copyB] == nil || len(r.b.watches) != 2 {
		t.Fatalf("watches after replacing = %d, want the host and copy b", len(r.b.watches))
	}
	for range watchPolls + 1 {
		r.b.Poll()
	}
	if n := len(r.b.watches); n != 0 {
		t.Errorf("watches after %d quiet polls = %d, want 0", watchPolls+1, n)
	}
	if !closed(host.file) {
		t.Error("the descriptor of the host was left open after its watch expired")
	}
}

func TestReplace_KeepsARefreshWrittenIntoTheReplacedInodeOfTheHost(t *testing.T) {
	r := hostOlderRig(t)
	global, err := filepath.EvalSymlinks(r.global)
	if err != nil {
		t.Fatal(err)
	}
	var codex *os.File
	r.b.hook = func(stage, path string) {
		if stage != "publish" || path != global {
			return
		}
		f, err := os.OpenFile(global, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // temporary dir
		if err != nil {
			t.Error(err)
			return
		}
		codex = f
	}
	r.b.Poll()
	if codex == nil {
		t.Fatal("the host was never replaced")
	}
	newest := authAt(10*time.Minute, "codex")
	if _, err := codex.Write(newest); err != nil {
		t.Fatal(err)
	}
	if err := codex.Close(); err != nil {
		t.Fatal(err)
	}
	r.b.Poll()
	for name, p := range map[string]string{"global": r.global, "copy a": r.copyA, "copy b": r.copyB} {
		if got := contentOf(t, p); got != string(newest) {
			t.Errorf("%s = %q, want the refresh Codex wrote into the replaced inode of the host", name, got)
		}
	}
}

func TestUntrack_OfTheLastRunClosesEveryDescriptor(t *testing.T) {
	r := hostOlderRig(t)
	r.b.Poll()
	var files []*os.File
	for _, w := range r.b.watches {
		files = append(files, w.file)
	}
	if len(files) != 2 {
		t.Fatalf("watches = %d, want 2", len(files))
	}
	r.b.Untrack("a")
	if len(r.b.watches) == 0 {
		t.Fatal("the host is still in use by run b; its watch should stay")
	}
	r.b.Untrack("b")
	if n := len(r.b.watches); n != 0 {
		t.Errorf("watches after the last untrack = %d, want 0", n)
	}
	for _, f := range files {
		if !closed(f) {
			t.Errorf("descriptor %s left open", f.Name())
		}
	}
}
