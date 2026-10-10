// Package client is the SDK that every interface (CLI, TUI, MCP) uses to talk
// to the Controller over mutual TLS with the CA pinned at connect time.
package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	requestTimeout = 30 * time.Second
	maxResponse    = 1 << 20
)

// Client calls the Controller API.
type Client struct {
	base       string
	http       *http.Client
	controller string
	dir        string // credentials directory; empty for a pre-enrolment client
}

// Open loads the credentials in dir and returns a client authenticated with them.
func Open(dir string) (*Client, error) {
	creds, err := LoadCredentials(dir)
	if err != nil {
		return nil, err
	}
	m, err := tlsMaterial(creds)
	if err != nil {
		return nil, err
	}
	c, err := newClient(creds.Controller, m.ca, []tls.Certificate{m.keyPair})
	if err != nil {
		return nil, err
	}
	c.dir = dir
	return c, nil
}

// newClient pins ca as the only root: the system store plays no part.
func newClient(controller string, ca *x509.Certificate, certs []tls.Certificate) (*Client, error) {
	host, _, err := net.SplitHostPort(controller)
	if err != nil {
		return nil, fmt.Errorf("client: Controller address %q: %w", controller, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &Client{
		base:       "https://" + controller,
		controller: controller,
		http: &http.Client{
			Timeout: requestTimeout,
			// A redirect could carry a token or certificate elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:   tls.VersionTLS13,
					RootCAs:      roots,
					ServerName:   host,
					Certificates: certs,
				},
			},
		},
	}, nil
}

// Controller is the host:port this client talks to.
func (c *Client) Controller() string { return c.controller }

// CreateToken asks for a join token and returns the invitation.
func (c *Client) CreateToken(ctx context.Context, kind domain.Kind, ttl time.Duration) (v1.CreateTokenResponse, error) {
	var out v1.CreateTokenResponse
	err := c.do(ctx, http.MethodPost, "/v1/tokens",
		v1.CreateTokenRequest{Kind: string(kind), TTLSeconds: int64(ttl / time.Second)}, &out)
	return out, err
}

// Clients lists the client identities.
func (c *Client) Clients(ctx context.Context) ([]v1.Client, error) {
	var out v1.ListClientsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/clients", nil, &out); err != nil {
		return nil, err
	}
	return out.Clients, nil
}

// RemoveClient revokes a client identity.
func (c *Client) RemoveClient(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/clients/"+url.PathEscape(id), nil, nil)
}

// Renew swaps this machine's certificate for a fresh one. The key is
// regenerated and never leaves the machine; the bundle is replaced atomically.
func (c *Client) Renew(ctx context.Context) error {
	if c.dir == "" {
		return errors.New("client: renew needs credentials opened with Open")
	}
	creds, err := LoadCredentials(c.dir)
	if err != nil {
		return err
	}
	m, err := tlsMaterial(creds)
	if err != nil {
		return err
	}
	key, csr, err := GenerateKeyAndCSR()
	if err != nil {
		return err
	}
	var out v1.CertificateResponse
	if err := c.do(ctx, http.MethodPost, "/v1/identity/renew", v1.RenewRequest{CSR: csr}, &out); err != nil {
		return err
	}
	keyPEM, err := EncodeKey(key)
	if err != nil {
		return err
	}
	if err := verifyIssued([]byte(out.Certificate), key, m.ca); err != nil {
		return err
	}
	creds.CertPEM, creds.KeyPEM = []byte(out.Certificate), keyPEM
	return SaveCredentials(c.dir, creds)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("client: encoding request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("client: building request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return c.transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return domain.NewError(domain.CodeInternal, "the Controller sent an unreadable response", nil)
	}
	return nil
}

func decodeError(status int, raw []byte) error {
	var env v1.ErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
		return domain.NewError(domain.CodeInternal, fmt.Sprintf("unexpected response from the Controller (HTTP %d)", status), nil)
	}
	return domain.NewError(env.Error.Code, env.Error.Message, env.Error.Details)
}

const renewHint = "ask the owner for a new invitation (`cf controller token create`) and run `cf connect`"

// transportError sorts connection failures: a certificate that one side
// refuses is an authentication problem, anything else is an unreachable Controller.
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return domain.NewError(domain.CodeUnauthenticated,
			"the Controller's certificate is not trusted by the CA pinned at connect time", nil)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return domain.NewError(domain.CodeUnauthenticated,
			"the Controller rejected this client certificate (expired or revoked): "+renewHint, nil)
	}
	return domain.NewError(domain.CodeControllerUnreachable,
		fmt.Sprintf("cannot reach the Controller at %s", c.controller), map[string]any{"cause": err.Error()})
}

// GenerateKeyAndCSR makes a fresh P-256 key and the certificate request that
// proves possession of it. The key never leaves the machine.
func GenerateKeyAndCSR() (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("client: generating key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return nil, "", fmt.Errorf("client: creating CSR: %w", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// EncodeKey is the PEM form stored in the credentials bundle.
func EncodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("client: encoding key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func parseCertPEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("client: not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("client: parsing certificate: %w", err)
	}
	return cert, nil
}

// verifyIssued refuses a certificate that is not for our key, not signed by
// the pinned CA or not usable for client authentication.
func verifyIssued(certPEM []byte, key *ecdsa.PrivateKey, ca *x509.Certificate) error {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return err
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("client: the Controller issued a certificate for a different key")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("client: issued certificate does not verify against the pinned CA: %w", err)
	}
	return nil
}

func pemEncode(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}
