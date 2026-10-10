package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// testController is a real Controller API over real TLS on localhost, with an
// injectable clock for the Authority (tokens and issued certificates).
type testController struct {
	t         *testing.T
	db        *store.DB
	ca        *pki.CA
	authority *pki.Authority
	clock     *clock.Fake
	addr      string
	logs      *logBuffer
	owner     *client.Client
	ownerDir  string
}

// newController starts a Controller whose Authority clock begins at start.
// TLS verification always uses the real time, so a start in the past yields
// client certificates that are genuinely expired.
func newController(t *testing.T, start time.Time, opts ...func(*api.Config)) *testController {
	t.Helper()
	return newControllerWithRoutes(t, start, nil, opts...)
}

// newControllerWithRoutes also lets the test add routes (see addRoute) before
// the server starts.
func newControllerWithRoutes(t *testing.T, start time.Time, routes func(*api.Server), opts ...func(*api.Config)) *testController {
	t.Helper()
	tc := &testController{t: t, clock: clock.NewFake(start), logs: &logBuffer{}}
	dir := t.TempDir()

	var err error
	tc.db, err = store.Open(t.Context(), filepath.Join(dir, "controller.db"), store.Options{Clock: tc.clock})
	must(t, err)
	t.Cleanup(func() { _ = tc.db.Close() })

	tc.ca, err = pki.NewCA(time.Now())
	must(t, err)
	serverCert, err := tc.ca.LoadServerCert(filepath.Join(dir, "pki"), "127.0.0.1", time.Now())
	must(t, err)

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	tc.addr = ln.Addr().String()
	host, port, _ := net.SplitHostPort(tc.addr)

	tc.authority = pki.NewAuthority(pki.AuthorityConfig{
		DB: tc.db, CA: tc.ca, Clock: tc.clock, IDs: domain.NewIDGenerator(tc.clock.Now, rand.Reader),
		CertTTL: domain.Defaults().IdentityCertTTL, Host: host, Port: port,
	})
	cfg := api.Config{
		Authority: tc.authority, CA: tc.ca, ServerCert: serverCert,
		Store: tc.db, Clock: tc.clock, StartedAt: tc.clock.Now(), Build: buildinfo.Info{Version: "test", Commit: "abc123"},
		Logger: slog.New(slog.NewTextHandler(tc.logs, nil)),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	srv := api.New(cfg)
	if routes != nil {
		routes(srv)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	tc.ownerDir = filepath.Join(dir, "owner", "credentials")
	tc.owner = tc.bootstrap(tc.ownerDir)
	return tc
}

// bootstrap creates an owner-style client identity directly (no token).
func (tc *testController) bootstrap(credDir string) *client.Client {
	tc.t.Helper()
	key, csr, err := client.GenerateKeyAndCSR()
	must(tc.t, err)
	keyPEM, err := client.EncodeKey(key)
	must(tc.t, err)
	joined, err := tc.authority.BootstrapOwner(tc.t.Context(), csr, "owner")
	must(tc.t, err)
	must(tc.t, client.SaveCredentials(credDir, client.Credentials{
		Controller: tc.addr, CertPEM: joined.Certificate, KeyPEM: keyPEM, CAPEM: tc.ca.CertPEM(),
	}))
	c, err := client.Open(credDir)
	must(tc.t, err)
	return c
}

// enroll connects a new machine the normal way: invitation, pinned TLS, join.
func (tc *testController) enroll(name string) (*client.Client, string) {
	tc.t.Helper()
	res, err := tc.owner.CreateToken(tc.t.Context(), domain.KindClient, time.Hour)
	must(tc.t, err)
	dir := filepath.Join(tc.t.TempDir(), "credentials")
	_, err = client.Connect(tc.t.Context(), res.Invite, name, dir)
	must(tc.t, err)
	c, err := client.Open(dir)
	must(tc.t, err)
	return c, dir
}

// enrollWorker inserts a worker identity directly: worker enrolment is not
// available through the API yet, but authorization must already treat the kind.
func (tc *testController) enrollWorker() *http.Client {
	tc.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(tc.t, err)
	id := domain.NewIDGenerator(time.Now, rand.Reader).New(domain.KindWorker)
	serial := big.NewInt(time.Now().UnixNano())
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: string(id)},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, tc.ca.Cert, &key.PublicKey, tc.ca.Key)
	must(tc.t, err)
	must(tc.t, tc.db.WithTx(tc.t.Context(), func(tx *store.Tx) error {
		return tx.InsertIdentity(tc.t.Context(), store.Identity{
			ID: id, Kind: domain.KindWorker, Name: "w", CertSerial: serial.Text(16),
			NotAfter: time.Now().Add(time.Hour), CreatedAt: time.Now(),
		})
	}))
	return tc.httpClient(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
}

// httpClient is a raw HTTPS client pinned to this Controller's CA. With no
// certificate it is anonymous.
func (tc *testController) httpClient(certs ...tls.Certificate) *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(tc.ca.Cert)
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: certs,
	}}
	tc.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// httpClientFor builds a raw client from stored credentials.
func (tc *testController) httpClientFor(credDir string) *http.Client {
	tc.t.Helper()
	creds, err := client.LoadCredentials(credDir)
	must(tc.t, err)
	pair, err := tls.X509KeyPair(creds.CertPEM, creds.KeyPEM)
	must(tc.t, err)
	return tc.httpClient(pair)
}

// call performs a request and decodes the error envelope, if any.
func (tc *testController) call(hc *http.Client, method, path, body string) (int, v1.ErrorBody, []byte) {
	tc.t.Helper()
	return doRaw(tc.t, hc, "https://"+tc.addr+path, method, body, nil)
}

func doRaw(t *testing.T, hc *http.Client, url, method, body string, req func(*http.Request)) (int, v1.ErrorBody, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	must(t, err)
	if req != nil {
		req(r)
	}
	resp, err := hc.Do(r)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	must(t, err)
	var env v1.ErrorEnvelope
	if resp.StatusCode >= 300 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s %s: status %d with a non-envelope body %q", method, url, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, env.Error, raw
}

func csrPEM(t *testing.T, key any, tpl *x509.CertificateRequest) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, tpl, key)
	must(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code domain.Code) {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != code {
		t.Fatalf("got %v, want code %s", err, code)
	}
}
