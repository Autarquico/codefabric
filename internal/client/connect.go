package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const dialTimeout = 10 * time.Second

// Connected is the result of a successful Connect.
type Connected struct {
	IdentityID string
	Controller string
}

// pinError marks a Controller whose identity does not match the invitation.
type pinError struct{ reason string }

func (e *pinError) Error() string { return e.reason }

// Connect enrols this machine with the Controller named by an invitation and
// saves the credentials into credentialsDir. It sends nothing secret until
// the Controller has proved, against the fingerprint in the invitation, that
// it holds the pinned CA and that its certificate is valid for the invited host.
func Connect(ctx context.Context, invitation, name, credentialsDir string) (Connected, error) {
	inv, err := v1.ParseInvite(invitation)
	if err != nil {
		return Connected{}, domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	// Fail on an unsafe destination now: the token is spent by the join.
	if _, err := checkDir(credentialsDir); err != nil {
		return Connected{}, err
	}
	ca, err := pinCA(ctx, inv)
	if err != nil {
		return Connected{}, err
	}

	// From here on the standard verifier runs against the pinned CA alone.
	c, err := newClient(inv.Address(), ca, nil)
	if err != nil {
		return Connected{}, err
	}
	key, csr, err := GenerateKeyAndCSR()
	if err != nil {
		return Connected{}, err
	}
	var out v1.CertificateResponse
	if err := c.do(ctx, http.MethodPost, "/v1/join", v1.JoinRequest{Token: inv.Token, CSR: csr, Name: name}, &out); err != nil {
		return Connected{}, err
	}
	if err := verifyIssued([]byte(out.Certificate), key, ca); err != nil {
		return Connected{}, err
	}
	keyPEM, err := EncodeKey(key)
	if err != nil {
		return Connected{}, err
	}
	err = SaveCredentials(credentialsDir, Credentials{
		Controller: inv.Address(),
		CertPEM:    []byte(out.Certificate),
		KeyPEM:     keyPEM,
		CAPEM:      pemCert(ca),
	})
	if err != nil {
		return Connected{}, fmt.Errorf("the invitation was redeemed but the credentials could not be saved (%w): %s", err, renewHint)
	}
	return Connected{IdentityID: out.IdentityID, Controller: inv.Address()}, nil
}

// pinCA opens a TLS connection that exchanges no application data, only to
// learn which CA the Controller presents. The handshake is allowed to proceed
// without the system trust store because VerifyConnection does the real
// work: the chain must contain the CA whose fingerprint the invitation
// carries, and the leaf must verify against that CA alone, for the invited
// host, as a server certificate, now.
func pinCA(ctx context.Context, inv v1.Invite) (*x509.Certificate, error) {
	var pinned *x509.Certificate
	dialer := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			ServerName:         inv.Host,
			InsecureSkipVerify: true, //nolint:gosec // VerifyConnection below replaces the default verification
			// VerifyConnection, unlike VerifyPeerCertificate, also runs on resumed sessions.
			VerifyConnection: func(cs tls.ConnectionState) error {
				ca, err := verifyAgainstPin(cs.PeerCertificates, inv, time.Now())
				pinned = ca
				return err
			},
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", inv.Address())
	if err != nil {
		var pe *pinError
		if errors.As(err, &pe) {
			return nil, domain.NewError(domain.CodeUnauthenticated,
				"the Controller does not match the invitation: "+pe.reason+"; nothing was sent", nil)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.NewError(domain.CodeControllerUnreachable,
			fmt.Sprintf("cannot reach the Controller at %s", inv.Address()), map[string]any{"cause": err.Error()})
	}
	_ = conn.Close()
	return pinned, nil
}

func verifyAgainstPin(chain []*x509.Certificate, inv v1.Invite, now time.Time) (*x509.Certificate, error) {
	if len(chain) < 2 {
		return nil, &pinError{"the Controller did not present its CA"}
	}
	leaf := chain[0]

	var ca *x509.Certificate
	others := x509.NewCertPool()
	for _, cert := range chain[1:] {
		if ca == nil && cert.IsCA && v1.Fingerprint(cert.Raw) == inv.Fingerprint {
			ca = cert
			continue
		}
		others.AddCert(cert)
	}
	if ca == nil {
		return nil, &pinError{"no CA in its chain matches the fingerprint in the invitation"}
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: others,
		DNSName:       inv.Host, // VerifyHostname matches IP SANs when this is an IP literal
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime:   now,
	})
	if err != nil {
		return nil, &pinError{"its certificate is not valid for " + inv.Host + ": " + err.Error()}
	}
	return ca, nil
}

func pemCert(c *x509.Certificate) []byte {
	return pemEncode("CERTIFICATE", c.Raw)
}
