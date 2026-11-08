package daemon_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/support/flock"
)

const (
	sentinelSecret = "SENTINEL-secret-value-4f1c"
	blobName       = "data/blobs/sha256/ab/cd/abcd0000000000000000000000000000000000000000000000000000000000ab"
)

func write(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

// plantState puts into a Controller what a backup must carry, and what it must not.
func plantState(t *testing.T, p config.Paths) {
	t.Helper()
	write(t, filepath.Join(p.DataDir, strings.TrimPrefix(blobName, "data/")), "blob bytes", 0o600)
	repo := filepath.Join(p.RelayDir(), "project_X.git")
	write(t, filepath.Join(repo, "HEAD"), "ref: refs/heads/main\n", 0o600)
	write(t, filepath.Join(repo, "objects", "aa", "obj"), "object", 0o600)
	write(t, filepath.Join(repo, "refs", "heads", "main"), "aa\n", 0o600)
	write(t, filepath.Join(repo, "packed-refs"), "# pack-refs\n", 0o600)
	write(t, filepath.Join(p.RelayDir(), "tmp", "junk"), "scratch", 0o600)
	write(t, p.WorkerSecretsFile(), sentinelSecret, 0o600)
	write(t, filepath.Join(p.WorkerDataDir(), "state.db"), sentinelSecret, 0o600)
	write(t, filepath.Join(p.WorkerDataDir(), "runs", "r1", "output.log"), sentinelSecret, 0o600)
}

type entry struct {
	name string
	body []byte
	mode int64
}

func readBackup(t *testing.T, path string) []entry {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []entry
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{hdr.Name, body, hdr.Mode})
	}
}

func names(es []entry) []string {
	var n []string
	for _, e := range es {
		n = append(n, e.name)
	}
	return n
}

func find(t *testing.T, es []entry, name string) entry {
	t.Helper()
	for _, e := range es {
		if e.name == name {
			return e
		}
	}
	t.Fatalf("%s is not in the backup: %v", name, names(es))
	return entry{}
}

func backupOf(t *testing.T, p config.Paths, passphrase string) (daemon.BackupResult, []entry) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "backup.tar")
	res, err := daemon.Backup(t.Context(), daemon.BackupOptions{Paths: p, Out: out, Passphrase: passphrase})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	info, err := os.Stat(res.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file = %v, %v; want mode 0600", info, err)
	}
	return res, readBackup(t, res.Path)
}

func TestScenario_BackupOfALiveControllerIsValidAndHoldsNoSecrets(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	start(t, p)
	plantState(t, p)

	res, es := backupOf(t, p, "")

	if first := es[0].name; first != daemon.BackupManifestEntry {
		t.Fatalf("first entry = %s, want the manifest", first)
	}
	var m daemon.BackupManifest
	if err := json.Unmarshal(es[0].body, &m); err != nil || m.Format != daemon.BackupFormat || m.CA != daemon.BackupCAAbsent {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if res.CAIncluded || slices.Contains(names(es), daemon.BackupCAEntry) {
		t.Error("the CA went into a backup without a passphrase")
	}

	db := filepath.Join(t.TempDir(), "controller.db")
	if err := os.WriteFile(db, find(t, es, daemon.BackupDatabaseEntry).body, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := store.ReadBackupInfo(t.Context(), db)
	if err != nil || info.ControllerID != m.ControllerID || info.SchemaVersion != m.SchemaVersion {
		t.Fatalf("database copy info = %+v, %v; manifest %+v", info, err, m)
	}

	for _, want := range []string{blobName, "data/relay/project_X.git/objects/aa/obj", "config/controller.json"} {
		find(t, es, want)
	}
	caKey, err := os.ReadFile(filepath.Join(p.PKIDir(), "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		switch {
		case strings.HasPrefix(e.name, "data/worker"), strings.HasPrefix(e.name, "data/relay/tmp"),
			strings.Contains(e.name, "credentials"), strings.Contains(e.name, "secrets.json"), strings.Contains(e.name, "pki"):
			t.Errorf("%s must not be in a backup", e.name)
		}
		if bytes.Contains(e.body, []byte(sentinelSecret)) || bytes.Contains(e.body, caKey) {
			t.Errorf("%s holds a secret", e.name)
		}
		if e.mode&0o077 != 0 {
			t.Errorf("%s has mode %o", e.name, e.mode)
		}
	}
}

func TestBackup_RelayRefsComeBeforeObjects(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	plantState(t, p)

	_, es := backupOf(t, p, "")

	order := names(es)
	refs := slices.Index(order, "data/relay/project_X.git/refs/heads/main")
	objects := slices.Index(order, "data/relay/project_X.git/objects/aa/obj")
	if refs < 0 || objects < 0 || refs > objects {
		t.Fatalf("refs at %d, objects at %d; refs must come first", refs, objects)
	}
	if slices.Index(order, "data/relay/project_X.git/packed-refs") > objects {
		t.Error("packed-refs came after the objects")
	}
}

func TestBackup_WithAPassphraseCarriesTheCAEncrypted(t *testing.T) {
	p := newPaths(t)
	initController(t, p)

	res, es := backupOf(t, p, "correct horse")

	if !res.CAIncluded {
		t.Fatal("CAIncluded = false")
	}
	sealed := find(t, es, daemon.BackupCAEntry).body
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatal("the CA key is in the clear")
	}
	got, err := daemon.DecryptCA(bytes.NewReader(sealed), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(p.PKIDir(), "ca.key"))
	if got.KeyPEM != string(want) {
		t.Error("the decrypted CA key is not the Controller's")
	}
	if _, err := daemon.DecryptCA(bytes.NewReader(sealed), "wrong"); err == nil {
		t.Error("a wrong passphrase decrypted the CA")
	}
}

func TestBackup_WaitsForTheGarbageCollectorsLock(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	release, err := flock.Lock(p.GCLockFile())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := daemon.Backup(t.Context(), daemon.BackupOptions{Paths: p, Out: filepath.Join(t.TempDir(), "b.tar")})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Backup finished while the collector held gc.lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBackup_RefusesToOverwriteOrToWriteIntoTheDataDirectory(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	existing := filepath.Join(t.TempDir(), "b.tar")
	write(t, existing, "precious", 0o600)
	for _, out := range []string{existing, filepath.Join(p.DataDir, "b.tar")} {
		if _, err := daemon.Backup(t.Context(), daemon.BackupOptions{Paths: p, Out: out}); err == nil {
			t.Errorf("Backup to %s succeeded", out)
		}
	}
	got, _ := os.ReadFile(existing) //nolint:gosec // a path of the test
	if string(got) != "precious" {
		t.Error("an existing file was overwritten")
	}
}

func TestBackupCommand_ReadsThePassphraseFromAFileAndWarnsWithoutOne(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	t.Setenv("CF_CONFIG_DIR", p.ConfigDir)
	t.Setenv("CF_DATA_DIR", p.DataDir)
	dir := t.TempDir()
	run := func(args ...string) (string, error) {
		cmd := daemon.BackupCommand()
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		cmd.SetOut(io.Discard)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return stderr.String(), err
	}

	warn, err := run("--out", filepath.Join(dir, "plain.tar"))
	if err != nil || !strings.Contains(warn, "join again") {
		t.Fatalf("without a passphrase: %q, %v", warn, err)
	}

	pass := filepath.Join(dir, "pass")
	write(t, pass, "hunter2\n", 0o644)
	if _, err := run("--out", filepath.Join(dir, "a.tar"), "--passphrase-file", pass); err == nil {
		t.Error("a passphrase file readable by others was accepted")
	}
	if err := os.Chmod(pass, 0o600); err != nil {
		t.Fatal(err)
	}
	if warn, err := run("--out", filepath.Join(dir, "b.tar"), "--passphrase-file", pass); err != nil || warn != "" {
		t.Fatalf("with a passphrase file: %q, %v", warn, err)
	}
	if _, err := daemon.DecryptCA(bytes.NewReader(find(t, readBackup(t, filepath.Join(dir, "b.tar")), daemon.BackupCAEntry).body), "hunter2"); err != nil {
		t.Errorf("the newline of the file became part of the passphrase: %v", err)
	}
}
