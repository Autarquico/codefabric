package secretjit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// wantPrivate fails unless the delivery in dir is 0700 and its files 0600,
// all of this user.
func wantPrivate(t *testing.T, dir string) {
	t.Helper()
	live := filepath.Join(dir, liveName)
	err := filepath.WalkDir(live, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		want := filePerm
		if d.IsDir() {
			want = dirPerm
		} else if !info.Mode().IsRegular() {
			t.Errorf("%s is %v", p, info.Mode().Type())
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %04o, want %04o", p, info.Mode().Perm(), want)
		}
		if uid := info.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
			t.Errorf("%s belongs to uid %d", p, uid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeliver_LaysOutAPrivateDeliveryTheHarnessAndTheRedactorsRead(t *testing.T) {
	old := syscall.Umask(0) // the modes do not depend on the umask
	defer syscall.Umask(old)
	dir := runDir(t)
	deliver(t, dir, 1, v2)

	wantOnly(t, dir, liveName)
	wantPrivate(t, dir)
	wantLoad(t, dir, v2)
	resolve := Resolver(dir)
	for _, s := range v2 {
		if got, err := resolve(s.Ref); err != nil || got != string(s.Value) {
			t.Fatalf("resolve(%q) = %v", s.Ref, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, liveName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(v2)+1 {
		t.Errorf("secrets/ has %d entries, want %d", len(entries), len(v2)+1)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, liveName, manifestName)) //nolint:gosec // under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range v2 {
		for _, line := range strings.Split(string(s.Value), "\n") {
			if strings.Contains(string(manifest), line) {
				t.Fatalf("manifest.json holds a value: %s", manifest)
			}
		}
	}
}

func TestDeliver_IsFencedByAttempt(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 2, v1)

	if err := New().Deliver(dir, 1, v2, allow); !errors.Is(err, ErrStale) {
		t.Fatalf("an older attempt: %v, want ErrStale", err)
	}
	wantLoad(t, dir, v1)

	f := &faults{}
	if err := f.manager().Deliver(dir, 2, v1, allow); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.steps, func(s string) bool { return strings.HasPrefix(s, "write") || strings.HasPrefix(s, "rename") }) {
		t.Errorf("the same delivery again was rewritten: %q", f.steps)
	}

	deliver(t, dir, 3, v2)
	wantLoad(t, dir, v2)
	wantOnly(t, dir, liveName)
}

func TestDeliver_SameAttemptKeepsWhatWasRevoked(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	if err := New().Revoke(dir, nil); err != nil {
		t.Fatal(err)
	}
	deliver(t, dir, 1, v1)
	if _, err := Resolver(dir)(v1[0].Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("resolve = %v, want ErrNoAccess", err)
	}
}

func TestDeliver_VerifiesBeforeChangingAnythingAndBeforePublishing(t *testing.T) {
	errFenced := errors.New("the Run stopped")
	for _, failAt := range []int{1, 2} {
		dir := runDir(t)
		calls := 0
		verify := func() error {
			if calls++; calls == failAt {
				return errFenced
			}
			return nil
		}
		if err := New().Deliver(dir, 1, v1, verify); !errors.Is(err, errFenced) {
			t.Fatalf("verify failing at call %d: Deliver = %v", failAt, err)
		}
		wantOnly(t, dir)
	}

	// A fenced replacement leaves the delivery in place untouched.
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	calls := 0
	verify := func() error {
		if calls++; calls == 2 {
			return errFenced
		}
		return nil
	}
	if err := New().Deliver(dir, 2, v2, verify); !errors.Is(err, errFenced) {
		t.Fatalf("Deliver = %v", err)
	}
	wantOnly(t, dir, liveName)
	wantLoad(t, dir, v1)
}

func TestDeliver_RefusesWhatCannotBeRedactedOrIsNotARunsSecrets(t *testing.T) {
	long := make([]redact.Needle, domain.MaxRunSecrets+1)
	for i := range long {
		long[i] = redact.Needle{Ref: "r" + strings.Repeat("x", i), Value: []byte("a-long-enough-value")}
	}
	cases := map[string]struct {
		attempt uint64
		secrets []redact.Needle
	}{
		"no attempt":    {0, v1},
		"no secrets":    {1, nil},
		"short value":   {1, []redact.Needle{{Ref: "a", Value: []byte("short")}}},
		"short line":    {1, []redact.Needle{{Ref: "a", Value: []byte("a-long-first-line\nab")}}},
		"huge value":    {1, []redact.Needle{{Ref: "a", Value: make([]byte, domain.MaxSecretValueLen+1)}}},
		"bad ref":       {1, []redact.Needle{{Ref: "../a", Value: []byte("a-long-enough-value")}}},
		"repeated ref":  {1, []redact.Needle{v1[0], v1[0]}},
		"too many refs": {1, long},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := runDir(t)
			err := New().Deliver(dir, c.attempt, c.secrets, allow)
			if err == nil {
				t.Fatal("Deliver succeeded")
			}
			if strings.Contains(err.Error(), "a-long") || strings.Contains(err.Error(), "short\"") {
				t.Fatalf("the error says the value: %v", err)
			}
			wantOnly(t, dir)
		})
	}
}

func TestResolver_GivesTheHarnessOnlyWhatItMayRead(t *testing.T) {
	dir := runDir(t)
	if _, err := Resolver(dir)("api/token"); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("before delivery: %v", err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("Load before delivery: %v", err)
	}
	deliver(t, dir, 1, v1)
	resolve := Resolver(dir)
	if _, err := resolve("other"); !errors.Is(err, ErrNoAccess) || !strings.Contains(err.Error(), `"other"`) {
		t.Fatalf("an undelivered ref: %v", err)
	}

	if err := New().Revoke(dir, []string{v1[0].Ref, "unknown"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(v1[0].Ref); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("a revoked ref: %v", err)
	}
	noValue(t, func() error { _, err := resolve(v1[0].Ref); return err }())
	if got, err := resolve(v1[1].Ref); err != nil || got != string(v1[1].Value) {
		t.Fatalf("a ref still granted: %v", err)
	}
	wantLoad(t, dir, v1) // retention outlives access
	wantPrivate(t, dir)

	if err := New().Release(dir, allow); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("Load after release: %v", err)
	}
	if err := New().Revoke(dir, nil); err != nil {
		t.Fatalf("Revoke with nothing delivered: %v", err)
	}
}

// tamper changes a delivery the way a harness running as this user could.
func TestLoadAndResolver_RefuseWhatIsNotAPrivateRegularFileOfThisUser(t *testing.T) {
	file := func(dir string) string { return filepath.Join(dir, liveName, fileName(v1[0].Ref)) }
	cases := map[string]struct {
		tamper func(t *testing.T, dir string) error
		want   error
	}{
		"open directory":       {func(_ *testing.T, dir string) error { return chmod(filepath.Join(dir, liveName), 0o750) }, ErrUnsafe},
		"open value":           {func(_ *testing.T, dir string) error { return chmod(file(dir), 0o640) }, ErrUnsafe},
		"open manifest":        {func(_ *testing.T, dir string) error { return chmod(filepath.Join(dir, liveName, manifestName), 0o644) }, ErrUnsafe},
		"value is a symlink":   {func(t *testing.T, dir string) error { return relink(t, file(dir)) }, ErrUnsafe},
		"manifest is symlink":  {func(t *testing.T, dir string) error { return relink(t, filepath.Join(dir, liveName, manifestName)) }, ErrUnsafe},
		"secrets is a symlink": {func(t *testing.T, dir string) error { return relink(t, filepath.Join(dir, liveName)) }, ErrUnsafe},
		"value is a fifo": {func(_ *testing.T, dir string) error {
			if err := os.Remove(file(dir)); err != nil {
				return err
			}
			return syscall.Mkfifo(file(dir), 0o600)
		}, ErrUnsafe},
		"value is missing":    {func(_ *testing.T, dir string) error { return os.Remove(file(dir)) }, ErrCorrupt},
		"manifest is missing": {func(_ *testing.T, dir string) error { return os.Remove(filepath.Join(dir, liveName, manifestName)) }, ErrCorrupt},
		"manifest is garbage": {func(_ *testing.T, dir string) error {
			return os.WriteFile(filepath.Join(dir, liveName, manifestName), []byte(`{"schema":1,"refs":[{"ref":"LEAKED-TOKEN`), 0o600)
		}, ErrCorrupt},
		"manifest points elsewhere": {func(_ *testing.T, dir string) error {
			return os.WriteFile(filepath.Join(dir, liveName, manifestName), []byte(`{"schema":1,"attempt":1,"refs":[{"ref":"a","file":"../../x","harness":true}]}`), 0o600)
		}, ErrCorrupt},
		"value too large": {func(_ *testing.T, dir string) error {
			return os.WriteFile(file(dir), make([]byte, domain.MaxSecretValueLen+1), 0o600)
		}, ErrCorrupt},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := runDir(t)
			deliver(t, dir, 1, v1)
			if err := c.tamper(t, dir); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := Load(dir)
				if !errors.Is(err, c.want) {
					t.Errorf("Load = %v, want %v", err, c.want)
				}
				_, err = Resolver(dir)(v1[0].Ref)
				if !errors.Is(err, c.want) {
					t.Errorf("resolve = %v, want %v", err, c.want)
				}
				noValue(t, err)
				if err != nil && strings.Contains(err.Error(), "LEAKED") {
					t.Errorf("the error repeats the manifest: %v", err)
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("reading blocked")
			}
			// Whatever was done to it, the delivery is released.
			if err := New().Release(dir, allow); err != nil {
				t.Fatal(err)
			}
			wantOnly(t, dir)
		})
	}
}

// chmod sets the modes a harness could set, wider than this package allows.
func chmod(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode) //nolint:gosec // the unsafe mode under test
}

// relink replaces path with a symlink to a copy of what it was.
func relink(t *testing.T, path string) error {
	t.Helper()
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(path, moved); err != nil {
		return err
	}
	return os.Symlink(moved, path)
}

type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (f fakeInfo) Sys() any          { return &syscall.Stat_t{Uid: f.uid} }

func TestPrivate_RefusesAnotherOwnerOrWiderPermissions(t *testing.T) {
	me := uint32(os.Getuid()) //nolint:gosec // a uid fits
	if err := private("f", fakeInfo{mode: 0o600, uid: me}, filePerm); err != nil {
		t.Fatal(err)
	}
	if err := private("f", fakeInfo{mode: 0o400, uid: me}, filePerm); err != nil {
		t.Fatalf("narrower permissions: %v", err)
	}
	if err := private("f", fakeInfo{mode: 0o600, uid: me + 1}, filePerm); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("another owner: %v", err)
	}
	if err := private("f", fakeInfo{mode: 0o604, uid: me}, filePerm); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("readable by others: %v", err)
	}
}

func TestRelease_IsFencedAndIdempotent(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	errOpen := errors.New("the capture is still open")
	if err := New().Release(dir, func() error { return errOpen }); !errors.Is(err, errOpen) {
		t.Fatalf("Release = %v", err)
	}
	wantLoad(t, dir, v1)
	for range 2 {
		if err := New().Release(dir, allow); err != nil {
			t.Fatal(err)
		}
		wantOnly(t, dir)
	}
	if err := New().Release(filepath.Join(dir, "missing"), allow); err != nil {
		t.Fatalf("a run directory that is gone: %v", err)
	}
}

func TestSweep_ReleasesOnlyWhatNoRunNorSessionNeeds(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	mk := func(name string) string {
		dir := filepath.Join(runs, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	done := mk("done-e1")
	running := mk("running-e1")
	sessionAlive := mk("attached-e2")
	unknown := mk("unknown-e1")
	halfBuilt := mk("preparing-e1")
	notARun := mk("not-a-run")
	for _, d := range []string{done, running, sessionAlive, unknown, notARun} {
		deliver(t, d, 1, v1)
	}
	orphans(t, running)
	if err := os.MkdirAll(filepath.Join(halfBuilt, stagingName), 0o700); err != nil {
		t.Fatal(err)
	}

	errList := errors.New("tmux did not answer")
	releasable := func(run string, epoch uint64) (bool, error) {
		if epoch == 0 {
			t.Errorf("epoch 0 for %s", run)
		}
		return run != "running" && run != "preparing", nil
	}
	alive := func(session string) (bool, error) {
		switch session {
		case domain.SessionName("attached", 2):
			return true, nil
		case domain.SessionName("unknown", 1):
			return false, errList
		}
		return false, nil
	}
	err := New().Sweep(runs, releasable, alive)
	if !errors.Is(err, errList) {
		t.Fatalf("Sweep = %v, want the session error", err)
	}
	wantOnly(t, done)
	wantOnly(t, running, liveName)
	wantOnly(t, sessionAlive, liveName)
	wantOnly(t, unknown, liveName) // unknown is not "no session"
	wantOnly(t, halfBuilt)
	wantOnly(t, notARun, liveName)

	if err := New().Sweep(filepath.Join(t.TempDir(), "none"), releasable, alive); err != nil {
		t.Fatalf("no runs directory: %v", err)
	}
}

// Deliveries, releases and reads of one Run race; a reader sees nothing or
// a whole delivery of one attempt, never a mix.
func TestConcurrentDeliverReleaseAndLoadNeverShowAHalfDelivery(t *testing.T) {
	dir := runDir(t)
	sets := [][]redact.Needle{v1, v2}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := New()
			for i := range 20 {
				attempt := uint64(i*4 + w + 1) //nolint:gosec // small
				if err := m.Deliver(dir, attempt, sets[attempt%2], allow); err != nil && !errors.Is(err, ErrStale) {
					t.Errorf("Deliver: %v", err)
				}
				if i%5 == 0 {
					if err := m.Release(dir, allow); err != nil {
						t.Errorf("Release: %v", err)
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			got, err := Load(dir)
			if errors.Is(err, ErrNotDelivered) {
				continue
			}
			if err != nil {
				t.Errorf("Load: %v", err)
				return
			}
			if !sameNeedles(got, v1) && !sameNeedles(got, v2) {
				t.Errorf("Load = %q", got)
				return
			}
		}
	}()
	wg.Wait()
	if err := New().Release(dir, allow); err != nil {
		t.Fatal(err)
	}
	wantOnly(t, dir)
}
