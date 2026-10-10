package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCASaveLoadKeepsPrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	ca, err := NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Save(dir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "ca.key"): 0o600, filepath.Join(dir, "ca.pem"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v", path, info, err)
		}
	}
	loaded, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint() != ca.Fingerprint() {
		t.Error("fingerprint changed across save/load")
	}
	if !Exists(dir) || Exists(t.TempDir()) {
		t.Error("Exists is wrong")
	}
	if !ca.Cert.IsCA || ca.Cert.Subject.CommonName == "" || ca.Key.Curve != elliptic.P256() {
		t.Errorf("unexpected CA: %+v", ca.Cert.Subject)
	}
}

func TestLoadCARejectsAKeyThatDoesNotMatch(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	caA, _ := NewCA(time.Now())
	caB, _ := NewCA(time.Now())
	if err := caA.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := caB.Save(b); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(b, "ca.key"))                            //nolint:gosec // test paths under t.TempDir
	if err := os.WriteFile(filepath.Join(a, "ca.key"), raw, 0o600); err != nil { //nolint:gosec // test paths under t.TempDir
		t.Fatal(err)
	}
	if _, err := LoadCA(a); err == nil {
		t.Fatal("a CA certificate with someone else's key loaded")
	}
}

func TestServerCertIsReusedUntilItNeedsRenewal(t *testing.T) {
	dir := t.TempDir()
	ca, _ := NewCA(time.Now())
	now := time.Now()

	first, err := ca.LoadServerCert(dir, "10.1.2.3", now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(first.Certificate[0])
	if len(leaf.IPAddresses) != 1 || len(leaf.DNSNames) != 0 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || leaf.IsCA {
		t.Errorf("IP host: %+v", leaf)
	}
	if got := leaf.PublicKey.(*ecdsa.PublicKey).Curve; got != elliptic.P256() { //nolint:forcetypeassert // issued by this package
		t.Errorf("curve %v", got)
	}

	again, _ := ca.LoadServerCert(dir, "10.1.2.3", now.Add(24*time.Hour))
	if !bytes.Equal(first.Certificate[0], again.Certificate[0]) {
		t.Error("a valid certificate was reissued")
	}
	soon, _ := ca.LoadServerCert(dir, "10.1.2.3", now.Add(340*24*time.Hour)) // < 30 days left
	if bytes.Equal(first.Certificate[0], soon.Certificate[0]) {
		t.Error("a certificate about to expire was kept")
	}
	renamed, _ := ca.LoadServerCert(dir, "ctl.example", now)
	rl, _ := x509.ParseCertificate(renamed.Certificate[0])
	if len(rl.DNSNames) != 1 || rl.DNSNames[0] != "ctl.example" {
		t.Errorf("DNS host: %+v", rl.DNSNames)
	}

	otherCA, _ := NewCA(time.Now())
	swapped, _ := otherCA.LoadServerCert(dir, "ctl.example", now)
	if bytes.Equal(renamed.Certificate[0], swapped.Certificate[0]) {
		t.Error("a certificate signed by another CA was reused")
	}
}

func TestTokensAreRandomAndOnlyTheirHashIsStable(t *testing.T) {
	a, _ := newToken()
	b, _ := newToken()
	if a == b || len(a) < 43 {
		t.Errorf("tokens %q %q", a, b)
	}
	h := hashToken(a)
	if h != hashToken(a) || h == hashToken(b) || h == a {
		t.Error("hashToken is not a stable one-way function")
	}
}

func TestCleanName(t *testing.T) {
	if got, err := cleanName("  laptop  "); err != nil || got != "laptop" {
		t.Errorf("%q %v", got, err)
	}
	if got, _ := cleanName(""); got != "unnamed" {
		t.Errorf("%q", got)
	}
	for _, bad := range []string{"a\nb", "x\x00", "bob\u202egpj.exe", "zero\u200bwidth", string(make([]rune, 65))} {
		if _, err := cleanName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadCARefusesAnUnsafeKeyOrDirectory(t *testing.T) {
	saved := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "pki")
		ca, _ := NewCA(time.Now())
		if err := ca.Save(dir); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("a private CA loads", func(t *testing.T) {
		if _, err := LoadCA(saved(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a key open to others", func(t *testing.T) {
		dir := saved(t)
		if err := os.Chmod(filepath.Join(dir, "ca.key"), 0o640); err != nil { //nolint:gosec // the point is to loosen it
			t.Fatal(err)
		}
		if _, err := LoadCA(dir); err == nil {
			t.Fatal("loaded a group-readable key")
		}
	})
	t.Run("a directory open to others", func(t *testing.T) {
		dir := saved(t)
		if err := os.Chmod(dir, 0o750); err != nil { //nolint:gosec // the point is to loosen it
			t.Fatal(err)
		}
		if _, err := LoadCA(dir); err == nil {
			t.Fatal("loaded from a group-accessible directory")
		}
	})
	t.Run("a symlinked key", func(t *testing.T) {
		dir := saved(t)
		moved := filepath.Join(t.TempDir(), "elsewhere.key")
		if err := os.Rename(filepath.Join(dir, "ca.key"), moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, filepath.Join(dir, "ca.key")); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCA(dir); err == nil {
			t.Fatal("loaded a symlinked key")
		}
	})
	t.Run("a symlinked directory", func(t *testing.T) {
		dir := saved(t)
		link := filepath.Join(t.TempDir(), "pki")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCA(link); err == nil {
			t.Fatal("loaded through a symlinked directory")
		}
	})
}

func TestRequirePrivateRefusesAFileThatIsNotRegular(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "ca.key")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requirePrivate(fifo, secretFilePerms, false); err == nil {
		t.Fatal("a FIFO passed as a private file")
	}
}
