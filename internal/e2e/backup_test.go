//go:build e2e && (linux || darwin)

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// The scenarios of the spec backup-restore (§28), with real daemons. A host is
// a Box: the restored Controller runs in a box of its own, with another
// address, and what carries over from the old one is the backup file alone.

const backupPassphrase = "correct horse battery staple"

// A secret value assembled at run time, so that no scanner of the repository trips on it.
var (
	backupSentinel  = "sk-" + "e2eBACKUP" + strings.Repeat("s3ntinel", 4)
	backupSecretRef = "e2e/backup-sentinel" //nolint:gosec // a secret ref, not a secret
)

// restoredHost is the new machine of a Controller restored from a backup.
type restoredHost struct {
	Box        *Box
	Addr       string
	Controller *Proc
}

// writePassphraseFile is a file of mode 0600 with the passphrase, in the harness directory.
func (c *Cluster) writePassphraseFile() string {
	c.t.Helper()
	file := filepath.Join(c.H.Dir(), "passphrase")
	if err := os.WriteFile(file, []byte(backupPassphrase+"\n"), 0o600); err != nil {
		c.t.Fatal(err)
	}
	return file
}

// Backup is `cf backup` on the Controller's machine, to a file of the harness directory.
func (c *Cluster) Backup(name string, flags ...string) (file string, res Result) {
	c.t.Helper()
	file = filepath.Join(c.H.Dir(), name+".tar")
	res = c.CF(c.Ctl, append([]string{"backup", "--out", file}, flags...)...)
	if res.Code != 0 {
		c.t.Fatalf("e2e: %s", res)
	}
	return file, res
}

// Restore is `cf restore` of backup in a new box, advertising a new address.
// The Controller is not started.
func (c *Cluster) Restore(box, backup string, flags ...string) (*restoredHost, Result) {
	c.t.Helper()
	h := &restoredHost{Box: c.H.Box(box), Addr: freeAddr(c.t)}
	args := append([]string{"restore", backup, "--advertise", h.Addr, "--listen", h.Addr}, flags...)
	res := c.CF(h.Box, args...)
	if res.Code != 0 {
		c.t.Fatalf("e2e: %s", res)
	}
	return h, res
}

// Start runs the restored Controller and waits until the observer, rebound to
// it, is answered. `connect --rebind` checks the destination before it writes.
func (h *restoredHost) Start(c *Cluster) {
	c.t.Helper()
	h.Controller = h.Box.CF(RoleController, "controller", "controller", "run")
	c.Eventually("the restored Controller to answer the rebound observer", func() (bool, string) {
		if !h.Controller.Running() {
			c.t.Fatalf("e2e: the restored Controller exited:\n%s", c.Logs())
		}
		res := c.CF(c.Observer, "connect", "--rebind", h.Addr)
		return res.Code == 0, res.String()
	})
}

// Adopt makes the cluster's owner and Workers use the restored Controller
// after nothing but their address changed, and the old Controller is gone.
func (h *restoredHost) Adopt(c *Cluster) {
	c.t.Helper()
	c.mustCF(c.Ctl, "connect", "--rebind", h.Addr)
	api, err := client.Open(c.Ctl.Paths.CredentialsDir())
	if err != nil {
		c.t.Fatal(err)
	}
	c.API, c.Addr, c.Controller = api, h.Addr, h.Controller
	for _, w := range c.Workers {
		c.mustCF(w.Box, "worker", "set-controller", h.Addr)
		c.RestartWorker(w) // the daemon reads the address when it starts
	}
}

// fileDigests are the SHA-256 of every regular file under root, by relative path.
func fileDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(p) //nolint:gosec // a file of the test's own cluster
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		rel, _ := filepath.Rel(root, p)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// segmentFiller prints more than store.SegmentBytes, so that the Controller
// seals the output into a gzip segment of the blob store.
const segmentFiller = `yes "a line of output that fills a segment of the blob store" | head -n 25000
`

// runToEnd runs a Task whose harness is released at once, and waits for it to
// publish. prefix is a script that runs before it.
func runToEnd(c *Cluster, prefix string) v1.Run {
	c.t.Helper()
	token := c.Ledger.Token()
	created := c.Do(prefix + c.Ledger.Script(token))
	waitHarness(c, token)
	c.Ledger.Release(token)
	run := waitPublished(c, created.Run.ID)
	if run.State != domain.RunCompleted {
		c.t.Fatalf("Run = %s", RunStage(run))
	}
	return run
}

func TestScenario_Restore_OtherHostKeepsIdsAndDigestsAndWorkersOnlyChangeAddress(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1})
	before := runToEnd(c, segmentFiller)
	w := c.Workers[0]
	c.Eventually("the output of the Run to reach the Controller", func() (bool, string) {
		out := c.mustCF(c.Observer, "logs", "--run", before.ID).Stdout
		return strings.Contains(out, "e2e: finished"), out
	})
	oldLogs := c.mustCF(c.Observer, "logs", "--run", before.ID).Stdout

	file, _ := c.Backup("restore", "--passphrase-file", c.writePassphraseFile())
	oldBlobs := fileDigests(t, c.Ctl.Paths.DataDir+"/blobs")
	if len(oldBlobs) == 0 {
		t.Fatal("the Controller has no blob: the comparison of digests would prove nothing")
	}
	c.Controller.Kill9() // the old host is gone

	host, res := c.Restore("ctl2", file, "--passphrase-file", c.writePassphraseFile())
	if strings.Contains(res.Stderr, "join again") {
		t.Errorf("a backup with the CA warned of a new join: %s", res.Stderr)
	}
	if host.Addr == c.Addr {
		t.Fatalf("the restored Controller advertises the old address %s", host.Addr)
	}
	host.Start(c)
	host.Adopt(c)

	// Same IDs and digests.
	after := c.Run(c.Observer, before.ID)
	if after.ID != before.ID || after.TaskID != before.TaskID || after.State != before.State || after.Publication != before.Publication {
		t.Errorf("Run after the restore = %s, before = %s", RunStage(after), RunStage(before))
	}
	if got := c.mustCF(c.Observer, "logs", "--run", before.ID).Stdout; got != oldLogs {
		t.Errorf("output after the restore differs:\n%q\nbefore:\n%q", got, oldLogs)
	}
	newBlobs := fileDigests(t, host.Box.Paths.DataDir+"/blobs")
	if !sameDigests(oldBlobs, newBlobs) {
		t.Errorf("blobs of the backup %v, of the restored Controller %v", oldBlobs, newBlobs)
	}
	if ws := c.ListWorkers(c.Observer); len(ws) != 1 || ws[0].ID != w.ID {
		t.Errorf("Workers after the restore = %+v, want only %s", ws, w.ID)
	}

	// The Worker runs a new Run through the restored Controller, and the old one stays.
	next := runToEnd(c, "")
	if next.WorkerID != w.ID || next.ID == before.ID {
		t.Errorf("the new Run %s ran on %q, want a new Run on %s", next.ID, next.WorkerID, w.ID)
	}
	if got := c.Run(c.Observer, next.ID); got.ID != next.ID {
		t.Errorf("run show %s = %s", next.ID, RunStage(got))
	}
}

func sameDigests(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func runIDs(entries []v1.PsEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// backupContents are the entries of a backup tar, by name.
func backupContents(t *testing.T, file string) map[string][]byte {
	t.Helper()
	f, err := os.Open(file) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		raw, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = raw
	}
}

// gunzipped is raw un-gzipped, or nil if it is not gzip.
func gunzipped(raw []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil
	}
	return out
}

// relayObjects dumps every object of every bare repository of the relay in the
// backup, as `git cat-file --batch-all-objects --batch` prints them.
func relayObjects(t *testing.T, contents map[string][]byte) (dump []byte, repos int) {
	t.Helper()
	root := t.TempDir()
	for name, raw := range contents {
		if !strings.HasPrefix(name, "data/relay/") {
			continue
		}
		dst := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(name, "data/relay/")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repoDirs, _ := filepath.Glob(filepath.Join(root, "*.git"))
	for _, repo := range repoDirs {
		cmd := exec.CommandContext(t.Context(), "git", "--git-dir", repo, "cat-file", "--batch-all-objects", "--batch") //nolint:gosec // a test
		cmd.Env = gittest.Env(t.TempDir())
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git cat-file in %s: %v", path.Base(repo), err)
		}
		dump = append(dump, out...)
		repos++
	}
	return dump, repos
}

func TestScenario_Backup_HoldsNoSecretValueAnywhere(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1})
	w := c.Workers[0]
	if res := c.CFInput(w.Box, backupSentinel, "worker", "secrets", "set", backupSecretRef); res.Code != 0 {
		t.Fatalf("e2e: %s", res)
	}
	c.mustCF(c.Ctl, "project", "secrets", "allow", "e2e", backupSecretRef)
	c.Eventually("the Controller to know the secret of the Worker", func() (bool, string) {
		got, err := c.API.Worker(c.H.Context(), w.Name)
		if err != nil {
			return false, err.Error()
		}
		return slices.ContainsFunc(got.Secrets, func(s v1.SecretMeta) bool { return s.Ref == backupSecretRef }), fmt.Sprintf("%+v", got.Secrets)
	})
	// The Run prints the secret, which the output redacts, and commits a file.
	created := c.Do(`echo "token=$TOKEN"; echo ok > result.txt; `+segmentFiller+`echo "end token=$TOKEN"`, "--secret-env", "TOKEN="+backupSecretRef)
	run := waitPublished(c, created.Run.ID)
	if run.State != domain.RunCompleted {
		t.Fatalf("the Run that uses the secret: %s", RunStage(run))
	}
	c.Eventually("the output of the Run to reach the Controller", func() (bool, string) {
		out := c.mustCF(c.Observer, "logs", "--run", run.ID).Stdout
		return strings.Contains(out, "end token=[REDACTED:"+backupSecretRef+"]"), out
	})

	file, _ := c.Backup("secrets", "--passphrase-file", c.writePassphraseFile())
	contents := backupContents(t, file)

	// Everything the backup holds, as it is and gunzipped (blobs: the segments of logs and output).
	var gzipped int
	var redacted bool
	for name, raw := range contents {
		if bytes.Contains(raw, []byte(backupSentinel)) {
			t.Errorf("the secret is in %s", name)
		}
		if text := gunzipped(raw); text != nil {
			gzipped++
			if bytes.Contains(text, []byte(backupSentinel)) {
				t.Errorf("the secret is in %s, gunzipped", name)
			}
			redacted = redacted || bytes.Contains(text, []byte("[REDACTED:"+backupSecretRef+"]"))
		}
	}
	if gzipped == 0 || !redacted {
		t.Errorf("entries %v", slices.Sorted(maps.Keys(contents)))
		t.Errorf("%d gzip entries, redaction seen=%t: the search in the segments of the output would prove nothing", gzipped, redacted)
	}
	// The relay: its objects are compressed by git, so they are read by git.
	dump, repos := relayObjects(t, contents)
	if repos == 0 || !bytes.Contains(dump, []byte("e2e")) {
		t.Fatalf("%d repositories in the relay of the backup; the dump holds the Project's commit: %t", repos, bytes.Contains(dump, []byte("e2e")))
	}
	if bytes.Contains(dump, []byte(backupSentinel)) {
		t.Error("the secret is in an object of the relay")
	}
	// The database.
	if db := contents["data/controller.db"]; len(db) == 0 {
		t.Error("the backup holds no database")
	}
	// The entries that would carry credentials are not there at all.
	for name := range contents {
		if strings.HasPrefix(name, "data/worker/") || strings.HasPrefix(name, "data/pki/") || strings.Contains(name, "credentials") || strings.Contains(name, "secrets.json") {
			t.Errorf("the backup holds %s", name)
		}
	}
}

func TestScenario_Restore_WithoutPassphraseTheBackupHasNoCAAndRestoreSaysToJoinAgain(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1})
	w := c.Workers[0]
	oldWorker := w.ID
	oldCA, err := os.ReadFile(filepath.Join(c.Ctl.Paths.PKIDir(), "ca.pem")) //nolint:gosec // the test's own cluster
	if err != nil {
		t.Fatal(err)
	}

	file, res := c.Backup("noca")
	if !strings.Contains(res.Stderr, "CA is not in this backup") {
		t.Errorf("backup said %q, want it to warn that the CA is missing", res.Stderr)
	}
	contents := backupContents(t, file)
	if _, ok := contents["pki/ca.age"]; ok {
		t.Error("a backup without passphrase holds an encrypted CA")
	}
	var manifest struct {
		CA string `json:"ca"`
	}
	if err := json.Unmarshal(contents["manifest.json"], &manifest); err != nil || manifest.CA != "absent" {
		t.Errorf("manifest.ca = %q (%v), want absent", manifest.CA, err)
	}
	for name, raw := range contents {
		if bytes.Contains(raw, []byte("PRIVATE KEY")) {
			t.Errorf("%s holds a private key", name)
		}
	}
	c.Controller.Kill9()

	host, restored := c.Restore("ctl2", file)
	for _, want := range []string{host.Addr, "token create", "connect --force", "worker join --force", "worker list", "worker remove"} {
		if !strings.Contains(restored.Stdout, want) {
			t.Errorf("restore said %q, want the next steps to mention %q", restored.Stdout, want)
		}
	}
	newCA, err := os.ReadFile(filepath.Join(host.Box.Paths.PKIDir(), "ca.pem")) //nolint:gosec // the test's own cluster
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldCA, newCA) {
		t.Error("the restored Controller has the old CA, which the backup did not hold")
	}

	// Changing the address is not enough: the Worker does not trust the new CA,
	// and its credentials are left as they were.
	host.Controller = host.Box.CF(RoleController, "controller", "controller", "run")
	c.Eventually("the restored Controller to be up", func() (bool, string) {
		ok := host.Controller.Running() && strings.Contains(host.Controller.Log()+host.Controller.Stderr(), host.Addr)
		return ok, host.Controller.Log() + host.Controller.Stderr()
	})
	controllerFile := filepath.Join(w.Box.Paths.WorkerCredentialsDir(), "controller")
	was, _ := os.ReadFile(controllerFile) //nolint:gosec // the test's own cluster
	res = c.CF(w.Box, "worker", "set-controller", host.Addr)
	if res.Code == 0 {
		t.Errorf("set-controller accepted a Controller with another CA: %s", res)
	}
	if strings.Contains(res.String(), "CF_POLICY_DENIED") {
		t.Errorf("set-controller was refused by policy, not because the CA is not the pinned one: %s", res)
	}
	if now, _ := os.ReadFile(controllerFile); !bytes.Equal(was, now) { //nolint:gosec // the test's own cluster
		t.Errorf("the address of the Worker changed to %q", now)
	}

	// The owner that restore left on the new host administers the Controller and
	// invites everyone back: a client and the Worker join again, and a Run goes through.
	c.mustCF(host.Box, "status")
	invite := func(kind string) string {
		return strings.TrimSpace(c.mustCF(host.Box, "controller", "token", "create", "--"+kind).Stdout)
	}
	c.mustCF(c.Observer, "connect", "--force", invite("client"), "--name", "observer")
	c.mustCF(c.Observer, "status")
	c.mustCF(c.Ctl, "connect", "--force", invite("client"), "--name", "owner-again") // the machine where the Tasks are made
	// The old record of the Worker is in the restored database under its name: the
	// steps that restore printed retire it, and the Worker joins under its own name.
	if listed := c.mustCF(host.Box, "worker", "list").Stdout; !strings.Contains(listed, w.Name) {
		t.Errorf("worker list = %q, want the old record of %s", listed, w.Name)
	}
	taken := c.CF(w.Box, "worker", "join", "--force", invite("worker"), "--name", w.Name, "--disk-min-free", "1MiB")
	if taken.Code == 0 || !strings.Contains(taken.String(), "--name") || !strings.Contains(taken.String(), "cf worker remove") {
		t.Errorf("join under a name taken: %s, want a refusal that names --name and cf worker remove", taken)
	}
	c.mustCF(host.Box, "worker", "remove", w.Name)
	c.mustCF(w.Box, "worker", "join", "--force", invite("worker"), "--name", w.Name, "--disk-min-free", "1MiB")
	api, err := client.Open(host.Box.Paths.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	c.API, c.Addr, c.Controller = api, host.Addr, host.Controller
	c.RestartWorker(w)
	for _, listed := range c.ListWorkers(c.Observer) {
		if listed.ID == oldWorker {
			t.Errorf("the old record of the Worker is still listed: %+v", listed)
		}
	}
	if next := runToEnd(c, ""); next.WorkerID != w.ID || next.WorkerID == oldWorker {
		t.Errorf("the Run after the re-join ran on %q, want the new Worker %s, not %s", next.WorkerID, w.ID, oldWorker)
	}
}

// A hot backup holds a Run in flight. Without the CA its Worker cannot return,
// so nothing may wait for it: the Run is lost, the reservation is released, and
// the steps restore prints retire the Worker and join it under its own name.
func TestScenario_Restore_WithoutCAAWorkerWithARunInFlightCanBeRetiredAndJoinAgain(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1})
	w := c.Workers[0]
	oldWorker := w.ID
	token := c.Ledger.Token()
	inFlight := c.Do(c.Ledger.Script(token)).Run.ID
	waitHarness(c, token)

	file, _ := c.Backup("inflight")
	c.Controller.Kill9()
	c.Ledger.Release(token)

	host, _ := c.Restore("ctl2", file)
	host.Controller = host.Box.CF(RoleController, "controller", "controller", "run")
	c.Eventually("the restored Controller to be up", func() (bool, string) {
		ok := host.Controller.Running() && strings.Contains(host.Controller.Log()+host.Controller.Stderr(), host.Addr)
		return ok, host.Controller.Log() + host.Controller.Stderr()
	})
	if shown := c.mustCF(host.Box, "run", "show", inFlight).Stdout; !strings.Contains(shown, "lost") {
		t.Errorf("run show %s = %q, want the Run lost", inFlight, shown)
	}
	invite := func() string {
		return strings.TrimSpace(c.mustCF(host.Box, "controller", "token", "create", "--worker").Stdout)
	}
	c.mustCF(host.Box, "worker", "remove", w.Name)
	c.mustCF(w.Box, "worker", "join", "--force", invite(), "--name", w.Name, "--disk-min-free", "1MiB")
	api, err := client.Open(host.Box.Paths.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	c.API, c.Addr, c.Controller = api, host.Addr, host.Controller
	c.RestartWorker(w)
	listed := c.mustCF(host.Box, "worker", "list").Stdout
	if !strings.Contains(listed, w.Name) || strings.Contains(listed, string(oldWorker)) {
		t.Errorf("worker list = %q, want %s under a new identity, not %s", listed, w.Name, oldWorker)
	}
	invitation := strings.TrimSpace(c.mustCF(host.Box, "controller", "token", "create", "--client").Stdout)
	c.mustCF(c.Ctl, "connect", "--force", invitation, "--name", "owner-again")
	// This Worker had orders under its old identity: the new one hears of its own.
	if next := runToEnd(c, ""); next.WorkerID == oldWorker {
		t.Errorf("the Run after the re-join ran on the old Worker %s", oldWorker)
	}
}

func TestScenario_Restore_RunsOfAnOlderBackupAreNotKnownByTheRestoredController(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1})
	w := c.Workers[0]
	file, _ := c.Backup("old", "--passphrase-file", c.writePassphraseFile())

	// After the backup: one Run ends and publishes, one is still running.
	ended := runToEnd(c, "")
	token := c.Ledger.Token()
	created := c.Do(c.Ledger.Script(token))
	live := created.Run.ID
	waitHarness(c, token)
	c.Controller.Kill9()

	host, _ := c.Restore("ctl2", file, "--passphrase-file", c.writePassphraseFile())
	host.Start(c)
	host.Adopt(c)

	// The Controller does not know them, and it did not hang: it answers and the Worker is online.
	for _, id := range []string{ended.ID, live} {
		if res := c.CF(c.Observer, "run", "show", id); res.Code == 0 {
			t.Errorf("the restored Controller knows Run %s, made after the backup: %s", id, res.Stdout)
		}
	}
	// The Worker kept the Runs: each is orphaned, or unknown to the Controller; none is lost or run twice.
	c.Eventually("the Worker to treat the Run the Controller does not know as orphaned or finished", func() (bool, string) {
		snap := c.WorkerSnapshot(w)
		for _, r := range snap.LocalRuns {
			if r.RunID == live {
				return r.Orphaned || r.Phase != "running", fmt.Sprintf("%+v", snap.LocalRuns)
			}
		}
		return false, fmt.Sprintf("%+v", snap.LocalRuns)
	})
	c.Ledger.Release(token)

	// The Controller is still sound: a new Run goes through the same Worker.
	// The Worker's cursor in the queue of orders is ahead of the one in the backup;
	// the restored Controller numbers its orders above it, so the Worker hears of this one.
	token = c.Ledger.Token()
	created = c.Do(c.Ledger.Script(token))
	started := false
	deadline := time.Now().Add(Scaled(10 * time.Second))
	for !started && time.Now().Before(deadline) {
		started = len(c.Ledger.Instances(token)) > 0
		time.Sleep(pollEvery)
	}
	if !started {
		t.Errorf("the new Run %s never started on the Worker after the restore: %s; worker snapshot %+v",
			created.Run.ID, RunStage(c.Run(c.Observer, created.Run.ID)), c.WorkerSnapshot(w))
		return
	}
	c.Ledger.Release(token)
	if next := waitPublished(c, created.Run.ID); next.WorkerID != w.ID || next.State != domain.RunCompleted {
		t.Errorf("the new Run: %s, want it completed on %s", RunStage(next), w.ID)
	}
	if got := runIDs(c.Ps(c.Observer)); slices.Contains(got, live) {
		t.Errorf("cf ps lists Run %s, which the restored Controller never knew: %v", live, got)
	}
}

// TestScenario_Restore_TheSameBackupTwiceInARow: the second restore, of the same
// file and after the Worker followed orders of the first, still reaches the Worker.
func TestScenario_Restore_TheSameBackupTwiceInARow(t *testing.T) {
	c := NewCluster(t, Opts{Workers: 1})
	w := c.Workers[0]
	file, _ := c.Backup("twice", "--passphrase-file", c.writePassphraseFile())
	c.Controller.Kill9()

	first, _ := c.Restore("ctl2", file, "--passphrase-file", c.writePassphraseFile())
	first.Start(c)
	first.Adopt(c)
	for range 2 {
		runToEnd(c, "")
	}
	first.Controller.Kill9()

	second, _ := c.Restore("ctl3", file, "--passphrase-file", c.writePassphraseFile())
	second.Start(c)
	second.Adopt(c)
	if next := runToEnd(c, ""); next.WorkerID != w.ID {
		t.Errorf("the Run after the second restore ran on %q, want %s", next.WorkerID, w.ID)
	}
}

// TestScenario_Restore_IntoTheEmptyDirectoriesOfANewMachine: `cf restore` is to
// accept a data and a config directory that exist and are empty (its help says
// "absent or empty"), as the directories of a machine set up with `mkdir`.
func TestScenario_Restore_IntoTheEmptyDirectoriesOfANewMachine(t *testing.T) {
	h := New(t)
	old := h.Box("old")
	run := func(b *Box, args ...string) Result {
		p := b.CF(RoleClient, "cf", args...)
		err := p.Wait(h.Context())
		res := Result{Args: args, Stdout: p.Log(), Stderr: p.Stderr()}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.Code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(old, "controller", "init", "--advertise", "127.0.0.1:7443", "--name", "owner"); res.Code != 0 {
		t.Fatalf("e2e: %s", res)
	}
	file := filepath.Join(h.Dir(), "empty.tar")
	if res := run(old, "backup", "--out", file); res.Code != 0 {
		t.Fatalf("e2e: %s", res)
	}
	fresh := h.Box("fresh") // its data and config directories exist, empty
	if res := run(fresh, "restore", file); res.Code != 0 {
		t.Errorf("restore into empty directories: %s", res)
	}
}
