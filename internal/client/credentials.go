package client

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	certFile       = "cert.pem"
	keyFile        = "key.pem"
	caFile         = "ca.pem"
	controllerFile = "controller"

	dirPerms  = 0o700
	filePerms = 0o600
)

// ErrUnsafeCredentials means the credentials directory is not private enough
// or is not a plain directory; nothing is read from or written over it.
var ErrUnsafeCredentials = errors.New("unsafe credentials directory")

// Credentials are everything a client needs to talk to its Controller. The CA
// is the one pinned at connect time, never a system root.
type Credentials struct {
	Controller string // host:port
	CertPEM    []byte
	KeyPEM     []byte
	CAPEM      []byte
}

// SaveCredentials publishes the bundle atomically. The files are written into
// <dir>.new (0700, files 0600), then the old directory is moved aside, the new
// one renamed into place and the old one deleted. A crash at any point leaves
// a complete bundle that LoadCredentials finds.
func SaveCredentials(dir string, c Credentials) error {
	if _, err := tlsMaterial(c); err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, dirPerms); err != nil {
		return fmt.Errorf("client: creating %s: %w", parent, err)
	}
	if err := recoverInterrupted(dir); err != nil {
		return err
	}
	exists, err := checkDir(dir)
	if err != nil {
		return err
	}

	staging, old := dir+".new", dir+".old"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("client: clearing %s: %w", staging, err)
	}
	if err := os.Mkdir(staging, dirPerms); err != nil {
		return fmt.Errorf("client: creating %s: %w", staging, err)
	}
	for name, content := range map[string][]byte{
		certFile: c.CertPEM, keyFile: c.KeyPEM, caFile: c.CAPEM, controllerFile: []byte(c.Controller + "\n"),
	} {
		if err := config.WriteFileAtomic(filepath.Join(staging, name), content, filePerms); err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
	}
	if err := config.SyncDir(staging); err != nil {
		return err
	}

	if exists {
		if err := os.RemoveAll(old); err != nil {
			return fmt.Errorf("client: clearing %s: %w", old, err)
		}
		if err := os.Rename(dir, old); err != nil {
			return fmt.Errorf("client: moving the old credentials aside: %w", err)
		}
	}
	if err := os.Rename(staging, dir); err != nil {
		if exists {
			_ = os.Rename(old, dir) // put the previous bundle back
		}
		return fmt.Errorf("client: publishing credentials: %w", err)
	}
	if err := config.SyncDir(parent); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

// LoadCredentials reads and validates the bundle. A missing bundle is
// CF_UNAUTHENTICATED with the way out; an unsafe one is ErrUnsafeCredentials.
func LoadCredentials(dir string) (Credentials, error) {
	if err := recoverInterrupted(dir); err != nil {
		return Credentials{}, err
	}
	exists, err := checkDir(dir)
	if err != nil {
		return Credentials{}, err
	}
	if !exists {
		return Credentials{}, domain.NewError(domain.CodeUnauthenticated,
			"no credentials found; run `cf connect <invitation>`", nil)
	}
	var c Credentials
	for name, dst := range map[string]*[]byte{certFile: &c.CertPEM, keyFile: &c.KeyPEM, caFile: &c.CAPEM} {
		if *dst, err = readPrivate(filepath.Join(dir, name)); err != nil {
			return Credentials{}, err
		}
	}
	addr, err := readPrivate(filepath.Join(dir, controllerFile))
	if err != nil {
		return Credentials{}, err
	}
	c.Controller = strings.TrimSpace(string(addr))
	if _, err := tlsMaterial(c); err != nil {
		return Credentials{}, err
	}
	return c, nil
}

// recoverInterrupted finishes a publish that crashed between moving the old
// directory aside and renaming the new one into place.
func recoverInterrupted(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("client: inspecting %s: %w", dir, err)
	}
	if err := os.Rename(dir+".old", dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("client: restoring previous credentials: %w", err)
	}
	return nil
}

// checkDir reports whether dir exists, and fails if it is a symlink, not a
// directory or readable by anyone but its owner.
func checkDir(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("client: inspecting %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("%w: %s is not a plain directory", ErrUnsafeCredentials, dir)
	}
	if info.Mode().Perm()&^dirPerms != 0 {
		return false, fmt.Errorf("%w: %s has mode %04o, want 0700", ErrUnsafeCredentials, dir, info.Mode().Perm())
	}
	return true, nil
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("client: reading credentials: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsafeCredentials, path)
	}
	if info.Mode().Perm()&^filePerms != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o, want 0600", ErrUnsafeCredentials, path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is inside the checked credentials directory
	if err != nil {
		return nil, fmt.Errorf("client: reading credentials: %w", err)
	}
	return raw, nil
}

type material struct {
	keyPair tls.Certificate
	ca      *x509.Certificate
}

// tlsMaterial checks that the bundle is coherent: the key matches the
// certificate, which is ECDSA and was issued by the pinned CA.
func tlsMaterial(c Credentials) (material, error) {
	pair, err := tls.X509KeyPair(c.CertPEM, c.KeyPEM)
	if err != nil {
		return material{}, fmt.Errorf("client: certificate and key do not match: %w", err)
	}
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		return material{}, errors.New("client: key is not ECDSA")
	}
	ca, err := parseCertPEM(c.CAPEM)
	if err != nil {
		return material{}, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return material{}, fmt.Errorf("client: parsing certificate: %w", err)
	}
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		return material{}, errors.New("client: certificate was not issued by the pinned CA")
	}
	if c.Controller == "" {
		return material{}, errors.New("client: missing Controller address")
	}
	return material{keyPair: pair, ca: ca}, nil
}

// Info describes the stored credentials without exposing any secret.
type Info struct {
	Controller string
	IdentityID string
	NotAfter   time.Time
}

// Describe reads the bundle in dir and summarises it.
func Describe(dir string) (Info, error) {
	creds, err := LoadCredentials(dir)
	if err != nil {
		return Info{}, err
	}
	m, err := tlsMaterial(creds)
	if err != nil {
		return Info{}, err
	}
	leaf, err := x509.ParseCertificate(m.keyPair.Certificate[0])
	if err != nil {
		return Info{}, fmt.Errorf("client: parsing certificate: %w", err)
	}
	return Info{Controller: creds.Controller, IdentityID: leaf.Subject.CommonName, NotAfter: leaf.NotAfter}, nil
}
