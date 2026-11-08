package daemon_test

import (
	"archive/tar"
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func controllerID(t *testing.T, p config.Paths) string {
	t.Helper()
	info, err := store.ReadBackupInfo(t.Context(), p.DatabaseFile())
	if err != nil {
		t.Fatal(err)
	}
	return info.ControllerID
}

// backupFile makes a backup of a fresh Controller with state planted in it.
func backupFile(t *testing.T, passphrase string) (config.Paths, string) {
	t.Helper()
	src := newPaths(t)
	initController(t, src)
	plantState(t, src)
	res, _ := backupOf(t, src, passphrase)
	return src, res.Path
}

// leavesNothing fails if a restore into p left any trace of itself.
func leavesNothing(t *testing.T, p config.Paths) {
	t.Helper()
	for _, dir := range []string{p.DataDir, p.ConfigDir} {
		if _, err := os.Lstat(dir); err == nil {
			t.Errorf("%s exists after a failed restore", dir)
		}
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".cf-restore-*"))
		if len(left) > 0 {
			t.Errorf("staging left behind: %v", left)
		}
	}
}

func TestScenario_RestoreOnAnotherHostKeepsIdsAndDigestsAndChangesTheAddress(t *testing.T) {
	src, backup := backupFile(t, "correct horse")
	dst := newPaths(t)
	newAddr := freeAddr(t)

	res, err := daemon.Restore(t.Context(), daemon.RestoreOptions{
		Paths: dst, Backup: backup, Advertise: newAddr, Listen: newAddr, Passphrase: "correct horse",
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if res.ControllerID != controllerID(t, src) || controllerID(t, dst) != res.ControllerID || !res.CARestored {
		t.Errorf("result = %+v; the Controller must be the same one", res)
	}
	for _, rel := range []string{
		filepath.Join("blobs", "sha256", "ab", "cd", "abcd0000000000000000000000000000000000000000000000000000000000ab"),
		filepath.Join("relay", "project_X.git", "objects", "aa", "obj"),
		filepath.Join("relay", "project_X.git", "refs", "heads", "main"),
	} {
		if got, want := readFile(t, filepath.Join(dst.DataDir, rel)), readFile(t, filepath.Join(src.DataDir, rel)); !bytes.Equal(got, want) {
			t.Errorf("%s differs", rel)
		}
	}
	srcInfo, _ := os.Stat(filepath.Join(src.DataDir, "blobs"))
	dstInfo, _ := os.Stat(filepath.Join(dst.DataDir, "blobs"))
	if !srcInfo.ModTime().Truncate(time.Second).Equal(dstInfo.ModTime()) { // tar keeps whole seconds
		t.Errorf("blobs mtime %v, want %v", dstInfo.ModTime(), srcInfo.ModTime())
	}
	for _, rel := range []string{"worker", filepath.Join("relay", "tmp")} {
		if _, err := os.Stat(filepath.Join(dst.DataDir, rel)); err == nil {
			t.Errorf("%s was restored", rel)
		}
	}

	cfg, err := config.LoadController(dst)
	if err != nil || cfg.Advertise != newAddr || cfg.Listen != newAddr {
		t.Fatalf("controller.json = %+v, %v", cfg, err)
	}
	oldCA, err := pki.LoadCA(src.PKIDir())
	if err != nil {
		t.Fatal(err)
	}
	newCA, err := pki.LoadCA(dst.PKIDir())
	if err != nil || newCA.Fingerprint() != oldCA.Fingerprint() {
		t.Fatalf("the restored CA is not the original: %v", err)
	}
	// The Workers pinned the old CA: the new server certificate must verify against it, for the new host.
	pool := x509.NewCertPool()
	pool.AddCert(oldCA.Cert)
	block, _ := pem.Decode(readFile(t, filepath.Join(dst.PKIDir(), "server.pem")))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "127.0.0.1", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("server certificate: %v", err)
	}
	for _, dir := range []string{dst.DataDir, dst.ConfigDir, dst.PKIDir()} {
		if m := mode(t, dir); m&0o077 != 0 {
			t.Errorf("%s has mode %o", dir, m)
		}
	}

	start(t, dst) // the restored Controller runs; the test stops it
}

func TestRestore_RefusesANonEmptyDestinationWithoutWritingAnything(t *testing.T) {
	_, backup := backupFile(t, "")
	for name, dirOf := range map[string]func(config.Paths) string{
		"data":   func(p config.Paths) string { return p.DataDir },
		"config": func(p config.Paths) string { return p.ConfigDir },
	} {
		t.Run(name, func(t *testing.T) {
			dst := newPaths(t)
			mine := filepath.Join(dirOf(dst), "mine")
			write(t, mine, "keep", 0o600)

			_, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup})

			if err == nil || !strings.Contains(err.Error(), "empty") {
				t.Fatalf("err = %v", err)
			}
			if string(readFile(t, mine)) != "keep" {
				t.Error("an existing file was touched")
			}
			for _, d := range []string{dst.DataDir, dst.ConfigDir} {
				entries, _ := os.ReadDir(d)
				if d != dirOf(dst) && len(entries) > 0 || len(entries) > 1 {
					t.Errorf("%s has %d entries", d, len(entries))
				}
			}
			left, _ := filepath.Glob(filepath.Join(filepath.Dir(dst.DataDir), ".cf-restore-*"))
			if len(left) > 0 {
				t.Errorf("staging left behind: %v", left)
			}
		})
	}
}

// tarOf writes a backup-shaped tar with a valid manifest followed by extra.
func tarOf(t *testing.T, extra ...tar.Header) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evil.tar")
	f, err := os.Create(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tw := tar.NewWriter(f)
	manifest, _ := json.Marshal(daemon.BackupManifest{Format: daemon.BackupFormat, CA: daemon.BackupCAAbsent, CreatedAt: time.Now()})
	all := append([]tar.Header{{Name: daemon.BackupManifestEntry, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(manifest))}}, extra...)
	for i, h := range all {
		body := []byte("x")
		if i == 0 {
			body = manifest
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScenario_RestoreRejectsAnArchiveThatWouldEscapeTheDestination(t *testing.T) {
	cases := map[string]tar.Header{
		"parent element":    {Name: "data/../../escaped", Typeflag: tar.TypeReg, Mode: 0o600},
		"leading parent":    {Name: "../escaped", Typeflag: tar.TypeReg, Mode: 0o600},
		"absolute":          {Name: "/tmp/escaped", Typeflag: tar.TypeReg, Mode: 0o600},
		"absolute in data":  {Name: "data//etc/escaped", Typeflag: tar.TypeReg, Mode: 0o600},
		"symlink out":       {Name: "data/blobs/link", Typeflag: tar.TypeSymlink, Linkname: "../../../escaped"},
		"symlink absolute":  {Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
		"hardlink out":      {Name: "data/hard", Typeflag: tar.TypeLink, Linkname: "../../escaped"},
		"hardlink inside":   {Name: "data/hard", Typeflag: tar.TypeLink, Linkname: "data/controller.db"},
		"device":            {Name: "data/dev", Typeflag: tar.TypeChar, Mode: 0o600},
		"fifo":              {Name: "data/fifo", Typeflag: tar.TypeFifo, Mode: 0o600},
		"unknown top level": {Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o600},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			dst := newPaths(t)
			_, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: tarOf(t, h)})
			if err == nil {
				t.Fatal("Restore accepted it")
			}
			leavesNothing(t, dst)
			for _, escaped := range []string{filepath.Join(filepath.Dir(dst.DataDir), "escaped"), filepath.Join(filepath.Dir(filepath.Dir(dst.DataDir)), "escaped")} {
				if _, err := os.Lstat(escaped); err == nil {
					t.Errorf("%s was written", escaped)
				}
			}
		})
	}
}

func TestRestore_RejectsDuplicatesAndAMissingManifestAndACorruptDatabase(t *testing.T) {
	dup := []tar.Header{
		{Name: "data/blobs/a", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "data/blobs/a", Typeflag: tar.TypeReg, Mode: 0o600},
	}
	garbage := []tar.Header{
		{Name: daemon.BackupDatabaseEntry, Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "config/controller.json", Typeflag: tar.TypeReg, Mode: 0o600},
	}
	for name, archive := range map[string]string{
		"duplicate":        tarOf(t, dup...),
		"corrupt database": tarOf(t, garbage...),
	} {
		t.Run(name, func(t *testing.T) {
			dst := newPaths(t)
			if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: archive}); err == nil {
				t.Fatal("Restore accepted it")
			}
			leavesNothing(t, dst)
		})
	}
	notTar := filepath.Join(t.TempDir(), "x")
	write(t, notTar, strings.Repeat("not a tar ", 100), 0o600)
	dst := newPaths(t)
	if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: notTar}); err == nil {
		t.Fatal("a file that is not a backup was accepted")
	}
	leavesNothing(t, dst)
}

func TestScenario_RestoreWithAWrongOrMissingPassphraseLeavesNothingBehind(t *testing.T) {
	_, backup := backupFile(t, "correct horse")
	for name, pass := range map[string]string{"wrong": "wrong horse", "missing": ""} {
		t.Run(name, func(t *testing.T) {
			dst := newPaths(t)
			_, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup, Passphrase: pass})
			if err == nil {
				t.Fatal("Restore succeeded")
			}
			leavesNothing(t, dst)
		})
	}
	dst := newPaths(t)
	if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup, Passphrase: "correct horse"}); err != nil {
		t.Fatalf("the right passphrase after the failures: %v", err)
	}
}

func TestScenario_RestoreWithoutACAWarnsThatWorkersMustJoinAgain(t *testing.T) {
	src, backup := backupFile(t, "")
	dst := newPaths(t)
	t.Setenv("CF_CONFIG_DIR", dst.ConfigDir)
	t.Setenv("CF_DATA_DIR", dst.DataDir)
	addr := freeAddr(t)

	cmd := daemon.RestoreCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{backup, "--advertise", addr})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !strings.Contains(stderr.String(), "join again") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if controllerID(t, dst) != controllerID(t, src) {
		t.Error("another Controller")
	}
	oldCA, _ := pki.LoadCA(src.PKIDir())
	newCA, err := pki.LoadCA(dst.PKIDir())
	if err != nil || newCA.Fingerprint() == oldCA.Fingerprint() {
		t.Errorf("expected a new CA: %v", err)
	}
	start(t, dst)
}

func TestRestoreCommand_ReadsThePassphraseFromAFile(t *testing.T) {
	_, backup := backupFile(t, "hunter2")
	dst := newPaths(t)
	t.Setenv("CF_CONFIG_DIR", dst.ConfigDir)
	t.Setenv("CF_DATA_DIR", dst.DataDir)
	pass := filepath.Join(t.TempDir(), "pass")
	write(t, pass, "hunter2\n", 0o600)

	cmd := daemon.RestoreCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{backup, "--passphrase-file", pass})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a backup with its CA warned: %q", stderr.String())
	}
}

func TestRestore_GoesIntoExistingEmptyDirectories(t *testing.T) {
	src, backup := backupFile(t, "")
	dst := newPaths(t)
	for _, d := range []string{dst.DataDir, dst.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup}); err != nil {
		t.Fatal(err)
	}
	if controllerID(t, dst) != controllerID(t, src) {
		t.Error("the restored Controller is not the one of the backup")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(dst.DataDir), ".cf-restore-*")); len(left) > 0 {
		t.Errorf("staging left behind: %v", left)
	}
}
