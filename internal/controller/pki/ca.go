package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

const (
	caValidity      = 10 * 365 * 24 * time.Hour
	clockSkew       = time.Minute
	caCertFile      = "ca.pem"
	caKeyFile       = "ca.key"
	pemTypeCert     = "CERTIFICATE"
	pemTypeECKey    = "EC PRIVATE KEY"
	pemTypeCertReq  = "CERTIFICATE REQUEST"
	secretFilePerms = 0o600
	secretDirPerms  = 0o700
)

// CA is the Controller's certificate authority. Its fingerprint is what
// invitations pin.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// NewCA creates a self-signed CA valid for ten years from now.
func NewCA(now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generating CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "CodeFabric Controller CA"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pki: self-signing CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parsing CA: %w", err)
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Fingerprint is the pin carried by invitations.
func (ca *CA) Fingerprint() string { return v1.Fingerprint(ca.Cert.Raw) }

// CertPEM is the public CA certificate.
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: ca.Cert.Raw})
}

// Save writes ca.key and ca.pem into dir (0700), each atomically with 0600.
// The key goes first, so a crash never leaves a CA certificate without its key.
func (ca *CA) Save(dir string) error {
	if err := os.MkdirAll(dir, secretDirPerms); err != nil {
		return fmt.Errorf("pki: creating %s: %w", dir, err)
	}
	keyPEM, err := encodeKey(ca.Key)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(dir, caKeyFile), keyPEM, secretFilePerms); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, caCertFile), ca.CertPEM(), secretFilePerms)
}

// LoadCA reads the CA saved by Save.
func LoadCA(dir string) (*CA, error) {
	cert, err := readCert(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	key, err := readKey(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("pki: CA certificate and key do not match")
	}
	if !cert.IsCA {
		return nil, errors.New("pki: CA certificate is not a CA")
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Exists reports whether any CA file is present in dir.
func Exists(dir string) bool {
	for _, f := range []string{caCertFile, caKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("pki: generating serial: %w", err)
	}
	return serial.Add(serial, big.NewInt(1)), nil // never zero
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("pki: encoding key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeECKey, Bytes: der}), nil
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is built from the data dir
	if err != nil {
		return nil, fmt.Errorf("pki: reading key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemTypeECKey {
		return nil, fmt.Errorf("pki: %s is not an EC private key", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parsing %s: %w", path, err)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("pki: %s is not P-256", path)
	}
	return key, nil
}

func readCert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is built from the data dir
	if err != nil {
		return nil, fmt.Errorf("pki: reading certificate: %w", err)
	}
	return parseCertPEM(raw)
}

// parseCertPEM decodes a single PEM certificate.
func parseCertPEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemTypeCert {
		return nil, errors.New("pki: not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parsing certificate: %w", err)
	}
	return cert, nil
}
