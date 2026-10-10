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

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
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
// a complete bundle that LoadCredentials finds: the previous one if it happened
// before the new bundle was complete, the new one if it happened after (a
// crash between the two renames is rolled forward).
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
		if err := atomicfile.Write(filepath.Join(staging, name), content, filePerms); err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
	}
	if err := atomicfile.SyncDir(staging); err != nil {
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
	if err := atomicfile.SyncDir(parent); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

// LoadCredentials reads and validates the bundle. A missing bundle is
// CF_UNAUTHENTICATED with the way out; an unsafe one is ErrUnsafeCredentials.
func LoadCredentials(dir string) (Credentials, error) {
	b, err := loadBundle(dir)
	return b.Credentials, err
}

// CredentialsExist reports whether dir already holds credentials. It is not a
// pure query: it first completes (renames on disk) a publish interrupted by a crash.
func CredentialsExist(dir string) (bool, error) {
	if err := recoverInterrupted(dir); err != nil {
		return false, err
	}
	_, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// bundle is a credentials bundle together with the TLS material validated
// from it, so callers parse nothing twice.
type bundle struct {
	Credentials
	material
}

func loadBundle(dir string) (bundle, error) {
	if err := recoverInterrupted(dir); err != nil {
		return bundle{}, err
	}
	exists, err := checkDir(dir)
	if err != nil {
		return bundle{}, err
	}
	if !exists {
		return bundle{}, domain.NewError(domain.CodeUnauthenticated,
			"no credentials found; run `cf connect <invitation>`", nil)
	}
	return readBundle(dir)
}

// readBundle reads dir, which checkDir has already vetted.
func readBundle(dir string) (bundle, error) {
	var c Credentials
	for name, dst := range map[string]*[]byte{certFile: &c.CertPEM, keyFile: &c.KeyPEM, caFile: &c.CAPEM} {
		raw, err := readPrivate(filepath.Join(dir, name))
		if err != nil {
			return bundle{}, err
		}
		*dst = raw
	}
	addr, err := readPrivate(filepath.Join(dir, controllerFile))
	if err != nil {
		return bundle{}, err
	}
	c.Controller = strings.TrimSpace(string(addr))
	m, err := tlsMaterial(c)
	if err != nil {
		return bundle{}, err
	}
	return bundle{Credentials: c, material: m}, nil
}

// recoverInterrupted finishes a publish that crashed between moving the old
// directory aside and renaming the new one into place. A complete <dir>.new is
// rolled forward; the previous bundle comes back only if there is none.
func recoverInterrupted(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("client: inspecting %s: %w", dir, err)
	}
	if stagedBundleIsComplete(dir + ".new") {
		if err := os.Rename(dir+".new", dir); err != nil {
			return fmt.Errorf("client: completing the interrupted publish: %w", err)
		}
		return os.RemoveAll(dir + ".old") // superseded: do not leave its key lying around
	}
	if err := os.Rename(dir+".old", dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("client: restoring previous credentials: %w", err)
	}
	return nil
}

func stagedBundleIsComplete(dir string) bool {
	if exists, err := checkDir(dir); err != nil || !exists {
		return false
	}
	_, err := readBundle(dir)
	return err == nil
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
	leaf    *x509.Certificate
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
	leaf := pair.Leaf // set by tls.X509KeyPair
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		return material{}, errors.New("client: certificate was not issued by the pinned CA")
	}
	if c.Controller == "" {
		return material{}, errors.New("client: missing Controller address")
	}
	return material{keyPair: pair, leaf: leaf, ca: ca}, nil
}

// Info describes the stored credentials without exposing any secret.
type Info struct {
	Controller string    `json:"controller"`
	IdentityID string    `json:"identity_id"`
	NotAfter   time.Time `json:"not_after"`
}

// Describe reads the bundle in dir and summarises it.
func Describe(dir string) (Info, error) {
	b, err := loadBundle(dir)
	if err != nil {
		return Info{}, err
	}
	return Info{Controller: b.Controller, IdentityID: b.leaf.Subject.CommonName, NotAfter: b.leaf.NotAfter}, nil
}
