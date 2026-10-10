package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	serverCertValidity = 365 * 24 * time.Hour
	serverRenewWindow  = 30 * 24 * time.Hour
	serverCertFile     = "server.pem"
	serverKeyFile      = "server.key"
)

// ParseCSR validates a client's certificate request before anything touches
// the database: a good signature (proof of possession) and an ECDSA P-256 key.
// Nothing else in the CSR is ever copied into a certificate.
func ParseCSR(csrPEM string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != pemTypeCertReq {
		return nil, invalidRequest("csr must be a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, invalidRequest("csr cannot be parsed")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, invalidRequest("csr signature is invalid")
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, invalidRequest("csr key must be ECDSA P-256")
	}
	return csr, nil
}

func invalidRequest(msg string) *domain.Error {
	return domain.NewError(domain.CodeInvalidRequest, msg, nil)
}

// signClient issues a client certificate. The template is entirely the
// Controller's: identity ID as CN, client-auth only, no SANs, no CA powers.
func (ca *CA) signClient(csr *x509.CertificateRequest, id domain.ID, now time.Time, ttl time.Duration) (der []byte, serial string, notAfter time.Time, err error) {
	sn, err := randomSerial()
	if err != nil {
		return nil, "", time.Time{}, err
	}
	notAfter = now.Add(ttl)
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: string(id)},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err = x509.CreateCertificate(rand.Reader, tpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("pki: signing client certificate: %w", err)
	}
	return der, sn.Text(16), notAfter, nil
}

// IssueServerCert creates a server certificate for host (a DNS name or IP),
// valid one year from now, and returns it as a TLS chain: leaf, then the CA.
func (ca *CA) IssueServerCert(host string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: generating server key: %w", err)
	}
	sn, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "CodeFabric Controller"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(serverCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else {
		tpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: signing server certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key}, nil
}

// LoadServerCert returns the server certificate saved in dir, issuing and
// saving a new one when it is missing, expires within 30 days or does not
// cover host.
func (ca *CA) LoadServerCert(dir, host string, now time.Time) (tls.Certificate, error) {
	if cert, ok := ca.readServerCert(dir, host, now); ok {
		return cert, nil
	}
	cert, err := ca.IssueServerCert(host, now)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := saveServerCert(dir, cert); err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

func (ca *CA) readServerCert(dir, host string, now time.Time) (tls.Certificate, bool) {
	leaf, err := readCert(filepath.Join(dir, serverCertFile))
	if err != nil {
		return tls.Certificate{}, false
	}
	key, err := readKey(filepath.Join(dir, serverKeyFile))
	if err != nil {
		return tls.Certificate{}, false
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) ||
		leaf.NotAfter.Sub(now) < serverRenewWindow ||
		leaf.VerifyHostname(host) != nil ||
		leaf.CheckSignatureFrom(ca.Cert) != nil {
		return tls.Certificate{}, false
	}
	return tls.Certificate{Certificate: [][]byte{leaf.Raw, ca.Cert.Raw}, PrivateKey: key}, true
}

func saveServerCert(dir string, cert tls.Certificate) error {
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("pki: server key is not ECDSA")
	}
	if err := os.MkdirAll(dir, secretDirPerms); err != nil {
		return fmt.Errorf("pki: creating %s: %w", dir, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, serverKeyFile), keyPEM, secretFilePerms); err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: cert.Certificate[0]})
	return config.WriteFileAtomic(filepath.Join(dir, serverCertFile), certPEM, secretFilePerms)
}
