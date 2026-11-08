package daemon_test

import (
	"archive/tar"
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
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
	if !srcInfo.ModTime().Round(time.Second).Equal(dstInfo.ModTime()) { // tar rounds to the nearest second
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

func TestScenario_RestoreWithoutACAExplainsHowToJoinAgain(t *testing.T) {
	src, backup := backupFile(t, "")
	dst := newPaths(t)
	t.Setenv("CF_CONFIG_DIR", dst.ConfigDir)
	t.Setenv("CF_DATA_DIR", dst.DataDir)
	addr := freeAddr(t)

	cmd := daemon.RestoreCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{backup, "--advertise", addr})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, want := range []string{addr, "cf controller run", "cf controller token create", "cf connect --force", "cf worker join --force", "cf worker list", "cf worker remove", dst.CredentialsDir()} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the next steps do not mention %q:\n%s", want, stdout.String())
		}
	}
	if controllerID(t, dst) != controllerID(t, src) {
		t.Error("another Controller")
	}
	oldCA, _ := pki.LoadCA(src.PKIDir())
	newCA, err := pki.LoadCA(dst.PKIDir())
	if err != nil || newCA.Fingerprint() == oldCA.Fingerprint() {
		t.Errorf("expected a new CA: %v", err)
	}
	assertOwnerAdministers(t, dst)
}

// assertOwnerAdministers starts the restored Controller and checks that the
// credentials left in its config directory reach /v1/status and create a join token.
func assertOwnerAdministers(t *testing.T, p config.Paths) {
	t.Helper()
	start(t, p)
	c, err := client.Open(p.CredentialsDir())
	if err != nil {
		t.Fatalf("the restore left no usable credentials: %v", err)
	}
	if _, err := c.Status(t.Context()); err != nil {
		t.Errorf("Status: %v", err)
	}
	for _, kind := range []domain.Kind{domain.KindClient, domain.KindWorker} {
		if _, err := c.CreateToken(t.Context(), kind, time.Hour); err != nil {
			t.Errorf("CreateToken(%s): %v", kind, err)
		}
	}
}

func TestScenario_RestoreWithTheCALeavesAnOwnerThatAdministersTheController(t *testing.T) {
	_, backup := backupFile(t, "correct horse")
	dst := newPaths(t)
	res, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup, Advertise: freeAddr(t), Passphrase: "correct horse"})
	if err != nil || !res.CARestored || res.OwnerID == "" {
		t.Fatalf("Restore = %+v, %v", res, err)
	}
	assertOwnerAdministers(t, dst)
}

func TestRestore_ListenFollowsTheAdvertisedPortAndIsValidated(t *testing.T) {
	src, backup := backupFile(t, "")
	oldCfg, err := config.LoadController(src)
	if err != nil {
		t.Fatal(err)
	}
	_, oldPort, _ := net.SplitHostPort(oldCfg.Listen)

	dst := newPaths(t)
	res, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup, Advertise: "127.0.0.1:28443"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Listen != "0.0.0.0:28443" {
		t.Errorf("listen = %q, want the advertised port on a wildcard (the backup's was on port %s, of a concrete IP)", res.Listen, oldPort)
	}
	if cfg, _ := config.LoadController(dst); cfg.Listen != res.Listen {
		t.Errorf("controller.json listen = %q, result %q", cfg.Listen, res.Listen)
	}

	for _, bad := range []string{"garbage", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999"} {
		other := newPaths(t)
		if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: other, Backup: backup, Listen: bad}); err == nil {
			t.Errorf("--listen %q was accepted", bad)
		}
		leavesNothing(t, other)
	}
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

func TestScenario_RestoreRefusesWhatBackupNeverWrites(t *testing.T) {
	reg := func(name string) tar.Header { return tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o700} }
	for name, h := range map[string]tar.Header{
		"relay hook":              reg("data/relay/hooks/pre-receive"),
		"relay hook update":       reg("data/relay/hooks/update"),
		"repository hook":         reg("data/relay/x.git/hooks/post-receive"),
		"repository hook dir":     {Name: "data/relay/x.git/hooks", Typeflag: tar.TypeDir, Mode: 0o700},
		"alternates":              reg("data/relay/x.git/objects/info/alternates"),
		"info attributes":         reg("data/relay/x.git/info/attributes"),
		"file in relay":           reg("data/relay/notes"),
		"unknown file of a repo":  reg("data/relay/x.git/gc.pid"),
		"credentials":             reg("config/credentials/ca.pem"),
		"worker secrets":          reg("config/worker/secrets.json"),
		"worker data":             reg("data/worker/state.db"),
		"server key":              reg("data/pki/server.key"),
		"database side file":      reg("data/controller.db-wal"),
		"a directory as database": {Name: "data/controller.db", Typeflag: tar.TypeDir, Mode: 0o700},
		"nameless repository":     reg("data/relay/.git/HEAD"),
		"other config":            reg("config/other.json"),
	} {
		t.Run(name, func(t *testing.T) {
			dst := newPaths(t)
			if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: tarOf(t, h)}); err == nil {
				t.Fatal("Restore accepted it")
			}
			leavesNothing(t, dst)
		})
	}
}

func TestScenario_RestoreRefusesAGitConfigThatRunsSomething(t *testing.T) {
	configOf := func(body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "evil.tar")
		f, err := os.Create(path) //nolint:gosec // a path of the test
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		tw := tar.NewWriter(f)
		manifest, _ := json.Marshal(daemon.BackupManifest{Format: daemon.BackupFormat, CA: daemon.BackupCAAbsent, CreatedAt: time.Now()})
		for _, e := range []struct {
			name string
			body []byte
		}{{daemon.BackupManifestEntry, manifest}, {"data/relay/x.git/config", []byte(body)}} {
			if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(e.body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, body := range map[string]string{
		"hooks path":      "[core]\n\thooksPath = /tmp/evil\n",
		"fsmonitor":       "[core]\n\tfsmonitor = /tmp/evil\n",
		"ssh command":     "[core]\n\tsshCommand = touch /tmp/x\n",
		"include":         "[include]\n\tpath = /tmp/evil\n",
		"subsection":      "[filter \"x\"]\n\tsmudge = evil\n",
		"continuation":    "[gc]\n\tauto = 0\\\n[core]\n",
		"key before head": "auto = 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			dst := newPaths(t)
			if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: configOf(body)}); err == nil || !strings.Contains(err.Error(), "config") {
				t.Fatalf("Restore = %v, want the config refused", err)
			}
			leavesNothing(t, dst)
		})
	}
}

func TestRestore_AsksForThePassphraseBeforeReadingTheRestOfTheBackup(t *testing.T) {
	_, backup := backupFile(t, "correct horse")
	dst := newPaths(t)
	_, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: backup})
	if err == nil || !strings.Contains(err.Error(), "--passphrase") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("err = %v", err)
	}
	leavesNothing(t, dst)
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

func TestScenario_ARelayRepositoryMadeByGitSurvivesBackupAndRestore(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	src := newPaths(t)
	initController(t, src)
	repo := filepath.Join(src.RelayDir(), "project_X.git")
	if out, err := exec.CommandContext(t.Context(), git, "init", "--bare", "--quiet", "--initial-branch=main", repo).CombinedOutput(); err != nil { //nolint:gosec // the test's own repository
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// What a git of an older template or a git that was interrupted leaves behind.
	write(t, filepath.Join(repo, "branches", "legacy"), "x", 0o600)
	write(t, filepath.Join(repo, "HEAD.lock"), "ref: refs/heads/main\n", 0o600)
	write(t, filepath.Join(repo, "refs", "heads", "main.lock"), "aa\n", 0o600)
	write(t, filepath.Join(repo, "FETCH_HEAD"), "", 0o600)

	res, _ := backupOf(t, src, "")
	dst := newPaths(t)
	if _, err := daemon.Restore(t.Context(), daemon.RestoreOptions{Paths: dst, Backup: res.Path, Advertise: freeAddr(t)}); err != nil {
		t.Fatalf("Restore refused a backup that Backup wrote: %v", err)
	}
	restored := filepath.Join(dst.RelayDir(), "project_X.git")
	if out, err := exec.CommandContext(t.Context(), git, "-C", restored, "symbolic-ref", "HEAD").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "refs/heads/main" { //nolint:gosec // the test's own repository
		t.Errorf("git cannot open the restored repository: %v\n%s", err, out)
	}
	for _, left := range []string{"branches", "HEAD.lock", "FETCH_HEAD", "hooks", filepath.Join("refs", "heads", "main.lock")} {
		if _, err := os.Lstat(filepath.Join(restored, left)); err == nil {
			t.Errorf("%s travelled in the backup", left)
		}
	}
}
