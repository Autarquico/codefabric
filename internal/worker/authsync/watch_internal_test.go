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

func descriptors(b *Broker) []*os.File {
	var files []*os.File
	for _, ws := range b.watches {
		for _, w := range ws {
			files = append(files, w.file)
		}
	}
	return files
}

func mustRelease(t *testing.T, b *Broker, id string) {
	t.Helper()
	if err := b.Release(id); err != nil {
		t.Fatal(err)
	}
}

func assertAllOpenOrClosed(t *testing.T, files []*os.File, wantClosed bool) {
	t.Helper()
	for _, f := range files {
		if closed(f) != wantClosed {
			t.Errorf("descriptor %s closed = %v, want %v", f.Name(), closed(f), wantClosed)
		}
	}
}

// truncOnPublish makes the Broker's next publication over path find Codex
// already holding it open with O_TRUNC, as Codex does before it writes.
func truncOnPublish(t *testing.T, r *watchRig, path string, into **os.File) {
	t.Helper()
	r.b.hook = func(stage, p string) {
		if stage != "publish" || p != path || *into != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // temporary dir
		if err != nil {
			t.Error(err)
			return
		}
		*into = f
	}
}

func writeAndClose(t *testing.T, f *os.File, content []byte) {
	t.Helper()
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A watch does not expire: the write lands after many polls and is still brought in.
func TestWatches_OutliveAnyNumberOfPolls(t *testing.T) {
	r := newWatchRig(t)
	var codex *os.File
	truncOnPublish(t, r, r.copyA, &codex)
	r.b.Poll()
	if codex == nil {
		t.Fatal("the copy was never replaced")
	}
	for range 10 {
		r.b.Poll()
	}
	if n := len(r.b.watches[r.copyA]); n != 1 {
		t.Fatalf("inodes of copy a after 10 quiet polls = %d, want 1", n)
	}
	newest := authAt(10*time.Minute, "late")
	writeAndClose(t, codex, newest)
	r.b.Poll()
	for name, p := range map[string]string{"global": r.global, "copy a": r.copyA, "copy b": r.copyB} {
		if got := contentOf(t, p); got != string(newest) {
			t.Errorf("%s = %q, want the late refresh", name, got)
		}
	}
}

// The inode of a first replacement is kept when the path is replaced again.
func TestWatches_AccumulateAcrossSuccessiveReplacements(t *testing.T) {
	r := newWatchRig(t)
	var codex *os.File
	truncOnPublish(t, r, r.copyA, &codex)
	r.b.Poll() // copy a replaced; codex holds the first inode
	if err := os.WriteFile(r.global, authAt(3*time.Minute, "g2"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.b.Poll() // copy a replaced again
	if got := contentOf(t, r.copyA); got != string(authAt(3*time.Minute, "g2")) {
		t.Fatalf("precondition: copy a holds %q, want the second replacement", got)
	}
	if n := len(r.b.watches[r.copyA]); n != 2 {
		t.Fatalf("inodes of copy a = %d, want 2", n)
	}
	for range 5 {
		r.b.Poll()
	}
	newest := authAt(10*time.Minute, "late")
	writeAndClose(t, codex, newest)
	r.b.Poll()
	for name, p := range map[string]string{"global": r.global, "copy a": r.copyA, "copy b": r.copyB} {
		if got := contentOf(t, p); got != string(newest) {
			t.Errorf("%s = %q, want the refresh written into the first inode", name, got)
		}
	}
}

// Past the bound the oldest inode is read before it is closed.
func TestWatches_AreBoundedAndTheOldestIsReadBeforeItIsClosed(t *testing.T) {
	r := newWatchRig(t)
	var codex *os.File
	truncOnPublish(t, r, r.copyA, &codex)
	r.b.Poll()
	first := r.b.watches[r.copyA][0].file
	// The pending write is made, visible to the Broker only when it evicts.
	newest := authAt(10*time.Minute, "late")
	if _, err := codex.Write(newest); err != nil {
		t.Fatal(err)
	}
	for i := range maxRetained {
		if err := os.WriteFile(r.global, authAt(time.Duration(3+i)*time.Second+2*time.Minute, "g"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The write is not seen by sync: keep the inode out of the reads by
		// making the Broker compare against what it holds.
		r.b.watches[r.copyA][0].last = newest
		r.b.Poll()
	}
	if n := len(r.b.watches[r.copyA]); n != maxRetained {
		t.Fatalf("inodes of copy a = %d, want the bound %d", n, maxRetained)
	}
	if !closed(first) {
		t.Error("the oldest inode was not closed at the bound")
	}
	if got := contentOf(t, r.copyA); got != string(newest) {
		t.Errorf("copy a = %q, want the content read from the oldest inode before it was closed", got)
	}
	if err := codex.Close(); err != nil {
		t.Fatal(err)
	}
}

// The host is watched while any Run is registered, and read in the
// reconciliation of the last.
func TestRelease_KeepsTheHostWatchUntilTheLastRunAndClosesEveryDescriptor(t *testing.T) {
	r := hostOlderRig(t)
	r.b.Poll()
	files := descriptors(r.b)
	if len(files) != 2 {
		t.Fatalf("descriptors = %d, want the host and copy b", len(files))
	}
	for range 10 {
		r.b.Poll()
	}
	mustRelease(t, r.b, "a")
	if len(r.b.watches) == 0 {
		t.Fatal("the host is still in use by run b; its watch should stay")
	}
	mustRelease(t, r.b, "b")
	if n := len(r.b.watches); n != 0 {
		t.Errorf("watches after the last release = %d, want 0", n)
	}
	assertAllOpenOrClosed(t, files, true)
}

// A refresh Codex writes into the replaced inode of the host after many polls
// reaches the copies at the release of the last Run.
func TestRelease_ReadsTheReplacedInodeOfTheHost(t *testing.T) {
	r := hostOlderRig(t)
	global, err := filepath.EvalSymlinks(r.global)
	if err != nil {
		t.Fatal(err)
	}
	var codex *os.File
	truncOnPublish(t, r, global, &codex)
	r.b.Poll()
	if codex == nil {
		t.Fatal("the host was never replaced")
	}
	for range 10 {
		r.b.Poll()
	}
	newest := authAt(10*time.Minute, "late")
	writeAndClose(t, codex, newest)
	mustRelease(t, r.b, "a")
	if got := contentOf(t, r.global); got != string(newest) {
		t.Errorf("host = %q, want the refresh found in its old inode", got)
	}
	mustRelease(t, r.b, "b")
	if n := len(descriptors(r.b)); n != 0 {
		t.Errorf("descriptors after the last release = %d", n)
	}
}

// What the copy of a Run holds when it is released, with no poll since, is kept.
func TestRelease_PropagatesTheLastRefreshOfTheCopy(t *testing.T) {
	r := newWatchRig(t)
	newest := authAt(10*time.Minute, "codex")
	if err := os.WriteFile(r.copyA, newest, 0o600); err != nil {
		t.Fatal(err)
	}
	mustRelease(t, r.b, "a")
	if got := contentOf(t, r.global); got != string(newest) {
		t.Errorf("host = %q, want the last refresh of the released Run", got)
	}
}

// A host that cannot be written fails the release, which leaves the Run
// registered so it can be tried again.
func TestRelease_FailsWhenTheHostCannotBeWritten(t *testing.T) {
	r := newWatchRig(t)
	newest := authAt(10*time.Minute, "codex")
	if err := os.WriteFile(r.copyA, newest, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(r.global)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // temporary dir
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // temporary dir
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	if err := r.b.Release("a"); err == nil {
		t.Fatal("Release succeeded though the host could not be written")
	}
	if _, ok := r.b.runs["a"]; !ok {
		t.Fatal("the Run was forgotten though its refresh was not kept")
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // temporary dir
		t.Fatal(err)
	}
	mustRelease(t, r.b, "a")
	if got := contentOf(t, r.global); got != string(newest) {
		t.Errorf("host = %q after the retry, want the refresh", got)
	}
}

// A refresh that lives only in an inode the Broker retained is not consumed by
// a Release whose host write failed: the retry publishes it.
func TestRelease_RetriesARefreshThatOnlyLivesInARetainedInode(t *testing.T) {
	r := newWatchRig(t)
	var codex *os.File
	r.b.hook = func(stage, path string) {
		if stage != "publish" || path != r.copyA || codex != nil {
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
	newest := authAt(10*time.Minute, "codex")
	if _, err := codex.Write(newest); err != nil {
		t.Fatal(err)
	}
	if err := codex.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(r.global)
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // temporary dir
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // temporary dir
	for range 2 {
		if err := r.b.Release("a"); err == nil {
			t.Fatal("Release succeeded though the host could not be written")
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // temporary dir
		t.Fatal(err)
	}
	mustRelease(t, r.b, "a")
	if got := contentOf(t, r.global); got != string(newest) {
		t.Errorf("host = %q after the retry, want the refresh of the retained inode", got)
	}
}

// A host that is momentarily not valid (Codex truncates before it writes) is a
// retryable error, and the Run, meanwhile, is a source and no destination.
func TestRelease_ATruncatedHostIsRetryableAndTheEndedRunIsNotWritten(t *testing.T) {
	r := newWatchRig(t)
	newest := authAt(10*time.Minute, "codex")
	if err := os.WriteFile(r.copyA, newest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.b.Release("a"); !errors.Is(err, errHostUnavailable) {
		t.Fatalf("Release = %v, want a retryable error", err)
	}
	if _, ok := r.b.runs["a"]; !ok {
		t.Fatal("the Run was forgotten though the host could not take its refresh")
	}

	// The host comes back with a version newer than the copy of the ended Run.
	later := authAt(20*time.Minute, "host")
	if err := os.WriteFile(r.global, later, 0o600); err != nil {
		t.Fatal(err)
	}
	r.b.Poll()
	if got := contentOf(t, r.copyA); got != string(newest) {
		t.Errorf("the copy of an ended Run was written: %q", got)
	}
	if got := contentOf(t, r.copyB); got != string(later) {
		t.Errorf("the live copy = %q, want the host's", got)
	}
	mustRelease(t, r.b, "a")
}

func TestRelease_AMissingHostIsNotAnError(t *testing.T) {
	r := newWatchRig(t)
	if err := os.Remove(r.global); err != nil {
		t.Fatal(err)
	}
	mustRelease(t, r.b, "a")
}

func TestOffer_PublishesTheCopyOfARunTheBrokerDoesNotKnow(t *testing.T) {
	r := newWatchRig(t)
	h, err := localhost.New(localhost.Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(t.TempDir(), "auth.json")
	newest := authAt(10*time.Minute, "stray")
	if err := os.WriteFile(stray, newest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.b.Offer(stamped{global: r.global}, h, []string{stray}); err != nil {
		t.Fatal(err)
	}
	if got := contentOf(t, r.global); got != string(newest) {
		t.Errorf("host = %q, want the copy of the stray Run", got)
	}
	if got := contentOf(t, r.copyA); got != string(newest) {
		t.Errorf("live copy = %q, want the refresh", got)
	}
}

func TestAbandon_ForgetsTheRunWithoutReconciling(t *testing.T) {
	r := newWatchRig(t)
	if err := os.WriteFile(r.copyA, authAt(10*time.Minute, "codex"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.b.Abandon("a")
	if _, ok := r.b.runs["a"]; ok {
		t.Error("the Run is still registered")
	}
	if got := contentOf(t, r.global); got != string(authAt(2*time.Minute, "g")) {
		t.Errorf("Abandon reconciled: host = %q", got)
	}
}
