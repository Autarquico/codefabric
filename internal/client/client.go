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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	requestTimeout  = 30 * time.Second
	idleConnTimeout = 90 * time.Second
	maxResponse     = 1 << 20
	// maxWorkflowResponse bounds the answers that carry a manifest. The
	// Controller stores only a manifest that serves in at most
	// v1.MaxServedManifestBytes; 2 MiB on top hold the rest of the version.
	maxWorkflowResponse = v1.MaxServedManifestBytes + 2<<20

	pemTypeCert    = "CERTIFICATE"
	pemTypeCertReq = "CERTIFICATE REQUEST"
	pemTypeECKey   = "EC PRIVATE KEY"
)

// Client calls the Controller API.
type Client struct {
	base       string
	controller string
	dir        string // credentials directory; empty for a pre-enrolment client
	recovery   string // what to do when the credentials are rejected

	roots      *x509.CertPool
	serverName string
	// http is replaced by Renew, which installs a transport that presents the
	// new certificate; renewMu keeps two renewals from racing over the bundle.
	http atomic.Pointer[http.Client]
	// stream is http over the same transport with no overall timeout: a stream
	// lasts as long as the Controller keeps it, and its own timers (streams.go)
	// bound the silence.
	stream  atomic.Pointer[http.Client]
	renewMu sync.Mutex

	clock  clock.Clock // the timers of streams
	timing streamTiming
}

// Option adjusts Open.
type Option func(*options)

type options struct {
	controller, recovery string
	clock                clock.Clock
}

// WithRecoveryHint replaces the advice given when the Controller rejects the
// credentials, for credentials that are not a client's.
func WithRecoveryHint(hint string) Option {
	return func(o *options) { o.recovery = hint }
}

// WithController dials addr (host:port) instead of the address stored in the
// bundle, for this client only: the bundle is not touched. The pinned CA and
// the client certificate stay, and the server certificate is still verified
// against addr's host, so it must be one that certificate covers.
func WithController(addr string) Option {
	return func(o *options) { o.controller = addr }
}

// WithClock gives the timers of the streams (the wait for headers, the silence
// watchdog and the reconnection backoff) another clock, for tests.
func WithClock(clk clock.Clock) Option {
	return func(o *options) { o.clock = clk }
}

// Open loads the credentials in dir and returns a client authenticated with them.
func Open(dir string, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	b, err := loadBundle(dir)
	if err != nil {
		return nil, err
	}
	controller := b.Controller
	if o.controller != "" {
		if !validControllerAddress(o.controller) {
			return nil, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("--controller %q is not a host:port address", o.controller), nil)
		}
		controller = o.controller
	}
	c, err := newClient(controller, b.ca, []tls.Certificate{b.keyPair})
	if err != nil {
		return nil, err
	}
	c.dir = dir
	if o.recovery != "" {
		c.recovery = o.recovery
	}
	if o.clock != nil {
		c.clock = o.clock
	}
	return c, nil
}

// validControllerAddress accepts a bare host and a numeric port from 1 to
// 65535: no scheme, credentials, path, query or fragment, which would otherwise
// bend the URL the client builds from it.
func validControllerAddress(addr string) bool {
	if strings.ContainsAny(addr, "/@?#") {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n >= 1
}

// newClient pins ca as the only root: the system store plays no part.
func newClient(controller string, ca *x509.Certificate, certs []tls.Certificate) (*Client, error) {
	host, _, err := net.SplitHostPort(controller)
	if err != nil {
		return nil, fmt.Errorf("client: Controller address %q: %w", controller, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	c := &Client{
		base: "https://" + controller, controller: controller, roots: roots, serverName: host, recovery: renewHint,
		clock: clock.Real{}, timing: defaultStreamTiming,
	}
	c.useCertificates(certs)
	return c, nil
}

// useCertificates points the client at a fresh transport presenting certs and
// closes the idle connections of the previous one, which carried the old
// certificate. Requests already in flight finish on the old transport.
func (c *Client) useCertificates(certs []tls.Certificate) {
	transport := &http.Transport{
		IdleConnTimeout: idleConnTimeout,
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			RootCAs:      c.roots,
			ServerName:   c.serverName,
			Certificates: certs,
		},
	}
	// A redirect could carry a token or certificate elsewhere.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	next := &http.Client{Timeout: requestTimeout, CheckRedirect: noRedirect, Transport: transport}
	nextStream := &http.Client{CheckRedirect: noRedirect, Transport: transport}
	c.stream.Store(nextStream)
	if prev := c.http.Swap(next); prev != nil {
		prev.CloseIdleConnections()
	}
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

// Status reports the Controller's identity, version, uptime and database health.
func (c *Client) Status(ctx context.Context) (v1.Status, error) {
	var out v1.Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
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

// Describe summarises the credentials this client was opened with, as they are
// stored now (after a Renew, the renewed ones).
func (c *Client) Describe() (Info, error) { return Describe(c.dir) }

// Renew swaps this machine's certificate for a fresh one. The key is
// regenerated and never leaves the machine; the bundle is replaced atomically
// and this client starts presenting the new certificate, so it keeps working.
// If the response is lost or the bundle cannot be written, the replaced
// certificate can renew again for a few minutes: run Renew again.
func (c *Client) Renew(ctx context.Context) error {
	if c.dir == "" {
		return errors.New("client: renew needs credentials opened with Open")
	}
	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	b, err := loadBundle(c.dir)
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
	if err := verifyIssued([]byte(out.Certificate), key, b.ca); err != nil {
		return err
	}
	b.CertPEM, b.KeyPEM = []byte(out.Certificate), keyPEM
	if err := SaveCredentials(c.dir, b.Credentials); err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return fmt.Errorf("client: renewed credentials are unusable: %w", err)
	}
	c.useCertificates([]tls.Certificate{pair})
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doLimited(ctx, maxResponse, method, path, in, out)
}

// doLimited is do for an answer of up to limit bytes.
func (c *Client) doLimited(ctx context.Context, limit int64, method, path string, in, out any) error {
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
	resp, err := c.http.Load().Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return c.transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.decodeError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return domain.NewError(domain.CodeInternal, "the Controller sent an unreadable response", nil)
	}
	return nil
}

// send makes a request whose body is raw bytes, not JSON (size is its length, or
// -1 if unknown), over hc, and returns the response of whatever status: the
// caller closes it. Auth, TLS and transport errors are those of do.
func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, body io.Reader, size int64) (*http.Response, error) {
	if body == nil || size == 0 {
		body, size = http.NoBody, 0
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("client: building request: %w", err)
	}
	if body != http.NoBody {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	return resp, nil
}

// sendOK is send for a caller that wants a 2xx: any other answer is decoded,
// as do decodes it, into a domain error and the body is closed.
func (c *Client) sendOK(ctx context.Context, hc *http.Client, method, path string, body io.Reader, size int64) (*http.Response, error) {
	resp, err := c.send(ctx, hc, method, path, body, size)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	return nil, c.decodeError(resp.StatusCode, raw)
}

// decodeError turns an error envelope into a domain error. An authentication
// failure of a client that has credentials gets the recovery hint: those
// credentials are expired, revoked or replaced.
func (c *Client) decodeError(status int, raw []byte) error {
	var env v1.ErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
		return domain.NewError(domain.CodeInternal, fmt.Sprintf("unexpected response from the Controller (HTTP %d)", status), nil)
	}
	msg := env.Error.Message
	if env.Error.Code == domain.CodeUnauthenticated && c.dir != "" {
		msg += ": " + c.recovery
	}
	return domain.NewError(env.Error.Code, msg, env.Error.Details)
}

const renewHint = "ask the owner for a new invitation (`cf controller token create`) and run `cf connect --force <invitation>`"

// WorkerRecoveryHint is the advice for a Worker whose credentials were rejected.
const WorkerRecoveryHint = "ask the owner for a new invitation (`cf controller token create --worker`) and run `cf worker join --force <invitation>`"

// transportError sorts connection failures: a certificate that one side
// refuses is an authentication problem, anything else (a name the server
// certificate does not cover included) is an unreachable Controller.
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cause := map[string]any{"cause": err.Error()} // local, so --debug may show it
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		var wrongHost x509.HostnameError
		if errors.As(verify.Err, &wrongHost) {
			return domain.NewError(domain.CodeControllerUnreachable,
				fmt.Sprintf("the Controller's certificate does not cover %q: dial an address it was issued for (see `cf controller init --advertise`)", c.serverName), cause)
		}
		return domain.NewError(domain.CodeUnauthenticated,
			"the Controller's certificate is not trusted by the CA pinned at connect time", cause)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" && rejectsCertificate(op.Err) {
		return domain.NewError(domain.CodeUnauthenticated,
			"the Controller rejected this client certificate (expired or revoked): "+c.recovery, cause)
	}
	return domain.NewError(domain.CodeControllerUnreachable,
		fmt.Sprintf("cannot reach the Controller at %s", c.controller), cause)
}

// TLS alerts (RFC 8446 §6.2) by which a server refuses a client certificate.
const (
	alertBadCertificate      = 42
	alertCertificateRevoked  = 44
	alertCertificateExpired  = 45
	alertUnknownCA           = 48
	alertCertificateRequired = 116
)

// rejectsCertificate tells whether err is the alert a server sends to refuse a
// client certificate. Over TCP, crypto/tls reports a received alert as a
// net.OpError whose Err is its unexported alert type (a uint8 holding the
// RFC number); tls.AlertError is only used by QUIC. The number is read by
// reflection because that is the one stable, typed part of the value. The
// unit test uses a stub; the real path is guarded by the Renovación scenario
// ("expired certificate asks for a new invitation"), which does a real handshake.
func rejectsCertificate(err error) bool {
	var code uint64
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		code = uint64(alertErr)
	} else if v := reflect.ValueOf(err); v.Kind() == reflect.Uint8 {
		code = v.Uint()
	} else {
		return false
	}
	switch code {
	case alertBadCertificate, alertCertificateRevoked, alertCertificateExpired, alertUnknownCA, alertCertificateRequired:
		return true
	}
	return false
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
	return key, string(pem.EncodeToMemory(&pem.Block{Type: pemTypeCertReq, Bytes: der})), nil
}

// EncodeKey is the PEM form stored in the credentials bundle.
func EncodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("client: encoding key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeECKey, Bytes: der}), nil
}

func parseCertPEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemTypeCert {
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
