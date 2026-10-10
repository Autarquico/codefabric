package client_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

func asDomain(err error, target **domain.Error) bool { return errors.As(err, target) }

func isUnsafe(err error) bool { return errors.Is(err, client.ErrUnsafeCredentials) }

// makeBundle signs a client certificate by hand: enough for credential tests,
// independent of the Controller.
func makeBundle(t *testing.T, ca *pki.CA, controller string) client.Credentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "client_X"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return client.Credentials{
		Controller: controller,
		CertPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:     pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CAPEM:      ca.CertPEM(),
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestCredentials_RoundTripWithPrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg", "credentials")
	want := makeBundle(t, newCA(t), "10.0.0.1:7443")
	if err := client.SaveCredentials(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("dir mode %04o", got)
	}
	for _, f := range []string{"cert.pem", "key.pem", "ca.pem", "controller"} {
		if got := mode(t, filepath.Join(dir, f)); got != 0o600 {
			t.Errorf("%s mode %04o", f, got)
		}
	}
	got, err := client.LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Controller != want.Controller || string(got.CertPEM) != string(want.CertPEM) || string(got.KeyPEM) != string(want.KeyPEM) {
		t.Errorf("round trip changed the bundle: %+v", got)
	}
	for _, leftover := range []string{dir + ".new", dir + ".old"} {
		if _, err := os.Lstat(leftover); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}
}

func TestCredentials_MissingBundleIsUnauthenticatedWithTheWayOut(t *testing.T) {
	_, err := client.LoadCredentials(filepath.Join(t.TempDir(), "credentials"))
	var de *domain.Error
	if !asDomain(err, &de) || de.Code != domain.CodeUnauthenticated || domain.ExitCode(de.Code) != 6 {
		t.Fatalf("err = %v", err)
	}
	if want := "cf connect"; !strings.Contains(de.Message, want) {
		t.Errorf("message %q does not mention %q", de.Message, want)
	}
}

func TestCredentials_UnsafeDirectoriesAndFilesAreRejected(t *testing.T) {
	good := makeBundle(t, newCA(t), "h:1")

	t.Run("symlinked directory", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		if err := client.SaveCredentials(target, good); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "credentials")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := client.LoadCredentials(link); !isUnsafe(err) {
			t.Errorf("load: %v", err)
		}
		if err := client.SaveCredentials(link, good); !isUnsafe(err) {
			t.Errorf("save: %v", err)
		}
		if _, err := client.Open(link); !isUnsafe(err) {
			t.Errorf("open: %v", err)
		}
	})

	t.Run("directory open to others", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := client.SaveCredentials(dir, good); err != nil {
			t.Fatal(err)
		}
		for _, m := range []os.FileMode{0o755, 0o750, 0o701} {
			if err := os.Chmod(dir, m); err != nil {
				t.Fatal(err)
			}
			if _, err := client.LoadCredentials(dir); !isUnsafe(err) {
				t.Errorf("mode %04o accepted: %v", m, err)
			}
			if err := client.SaveCredentials(dir, good); !isUnsafe(err) {
				t.Errorf("mode %04o overwritten: %v", m, err)
			}
		}
	})

	t.Run("file open to others", func(t *testing.T) {
		for _, name := range []string{"key.pem", "cert.pem", "ca.pem", "controller"} {
			dir := filepath.Join(t.TempDir(), "credentials")
			if err := client.SaveCredentials(dir, good); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, name), 0o644); err != nil { //nolint:gosec // opening the mode is the test
				t.Fatal(err)
			}
			if _, err := client.LoadCredentials(dir); !isUnsafe(err) {
				t.Errorf("%s at 0644 accepted: %v", name, err)
			}
		}
	})

	t.Run("file replaced by a symlink", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := client.SaveCredentials(dir, good); err != nil {
			t.Fatal(err)
		}
		elsewhere := filepath.Join(t.TempDir(), "key")
		if err := os.Rename(filepath.Join(dir, "key.pem"), elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(dir, "key.pem")); err != nil {
			t.Fatal(err)
		}
		if _, err := client.LoadCredentials(dir); !isUnsafe(err) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCredentials_AnIncoherentBundleNeverReplacesAGoodOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	ca := newCA(t)
	good := makeBundle(t, ca, "good:1")
	if err := client.SaveCredentials(dir, good); err != nil {
		t.Fatal(err)
	}

	foreign := makeBundle(t, newCA(t), "bad:1")
	mismatchedKey := good
	mismatchedKey.KeyPEM = makeBundle(t, ca, "x:1").KeyPEM
	wrongCA := good
	wrongCA.CAPEM = foreign.CAPEM
	noAddress := good
	noAddress.Controller = ""

	for name, bad := range map[string]client.Credentials{"key mismatch": mismatchedKey, "foreign CA": wrongCA, "no address": noAddress} {
		if err := client.SaveCredentials(dir, bad); err == nil {
			t.Errorf("%s: save succeeded", name)
		}
		got, err := client.LoadCredentials(dir)
		if err != nil || got.Controller != "good:1" {
			t.Fatalf("%s damaged the stored bundle: %+v, %v", name, got, err)
		}
	}
}

func TestCredentials_PartialFailuresAreRecovered(t *testing.T) {
	good := makeBundle(t, newCA(t), "old:1")
	next := makeBundle(t, newCA(t), "new:1")

	t.Run("crash while staging leaves the published bundle intact", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := client.SaveCredentials(dir, good); err != nil {
			t.Fatal(err)
		}
		// A half-written staging directory from a crashed run.
		if err := os.Mkdir(dir+".new", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir+".new", "key.pem"), []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := client.LoadCredentials(dir)
		if err != nil || got.Controller != "old:1" {
			t.Fatalf("load: %+v, %v", got, err)
		}
		if err := client.SaveCredentials(dir, next); err != nil {
			t.Fatal(err)
		}
		got, err = client.LoadCredentials(dir)
		if err != nil || got.Controller != "new:1" {
			t.Fatalf("after save: %+v, %v", got, err)
		}
		if _, err := os.Lstat(dir + ".new"); err == nil {
			t.Error("staging directory left behind")
		}
	})

	t.Run("crash between the two renames completes the new bundle", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := client.SaveCredentials(dir, good); err != nil {
			t.Fatal(err)
		}
		// The real crash: the new bundle is complete in <dir>.new and the old
		// directory was moved aside, but <dir>.new was not renamed to <dir>.
		if err := client.SaveCredentials(dir+".new", next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(dir, dir+".old"); err != nil {
			t.Fatal(err)
		}
		got, err := client.LoadCredentials(dir)
		if err != nil || got.Controller != "new:1" {
			t.Fatalf("load: %+v, %v", got, err)
		}
		for _, leftover := range []string{dir + ".new", dir + ".old"} {
			if _, err := os.Lstat(leftover); err == nil {
				t.Errorf("%s left behind", leftover)
			}
		}
	})

	t.Run("crash after moving the old bundle aside with no usable new one restores the old", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := client.SaveCredentials(dir, good); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(dir, dir+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir+".new", 0o700); err != nil { // half written
			t.Fatal(err)
		}
		got, err := client.LoadCredentials(dir)
		if err != nil || got.Controller != "old:1" {
			t.Fatalf("load: %+v, %v", got, err)
		}
		if _, err := os.Lstat(dir + ".old"); err == nil {
			t.Error("the old directory was not moved back")
		}
	})

	t.Run("a leftover old directory does not block the next save", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := client.SaveCredentials(dir, good); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir+".old", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := client.SaveCredentials(dir, next); err != nil {
			t.Fatal(err)
		}
		got, err := client.LoadCredentials(dir)
		if err != nil || got.Controller != "new:1" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
}
