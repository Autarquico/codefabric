package api_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestScenario_ControllerIdentityTLS_Renovacion(t *testing.T) {
	t.Run("valid certificate is replaced and the old one stops working", func(t *testing.T) {
		tc := newController(t, time.Now())
		b, dir := tc.enroll("laptop")

		stale := t.TempDir() + "/credentials"
		creds, err := client.LoadCredentials(dir)
		must(t, err)
		must(t, client.SaveCredentials(stale, creds))
		before, err := client.Describe(dir)
		must(t, err)

		must(t, b.Renew(t.Context()))

		after, err := client.Describe(dir)
		must(t, err)
		if after.IdentityID != before.IdentityID {
			t.Errorf("identity changed: %s -> %s", before.IdentityID, after.IdentityID)
		}
		renewed, err := client.Open(dir)
		must(t, err)
		if _, err := renewed.Clients(t.Context()); err != nil {
			t.Fatalf("renewed certificate: %v", err)
		}
		old, err := client.Open(stale)
		must(t, err)
		_, err = old.Clients(t.Context())
		wantCode(t, err, domain.CodeUnauthenticated)
	})

	t.Run("expired certificate asks for a new invitation", func(t *testing.T) {
		// The Authority's clock is 200 days behind: the owner's 90-day
		// certificate was already expired the moment it was issued.
		tc := newController(t, time.Now().Add(-200*24*time.Hour))
		err := tc.owner.Renew(t.Context())
		wantCode(t, err, domain.CodeUnauthenticated)
		if !strings.Contains(err.Error(), "new invitation") {
			t.Errorf("no hint in %q", err)
		}
		if got := domain.ExitCode(domain.CodeUnauthenticated); got != 6 {
			t.Errorf("exit code %d", got)
		}
	})
}

func TestControllerIdentityTLS_RevocationCutsPersistentConnection(t *testing.T) {
	tc := newController(t, time.Now())
	b, dir := tc.enroll("laptop")
	raw := tc.httpClientFor(dir)

	if code, _, _ := tc.call(raw, http.MethodGet, "/v1/clients", ""); code != http.StatusOK {
		t.Fatalf("before revocation: %d", code)
	}
	list, err := b.Clients(t.Context())
	must(t, err)
	var bID string
	for _, c := range list {
		if c.Name == "laptop" {
			bID = c.ID
		}
	}
	must(t, tc.owner.RemoveClient(t.Context(), bID))

	var reused bool
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}
	code, body, _ := doRaw(t, raw, "https://"+tc.addr+"/v1/clients", http.MethodGet, "", func(r *http.Request) {
		*r = *r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	})
	if !reused {
		t.Fatal("the second request did not reuse the connection; the test proves nothing")
	}
	if code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
		t.Fatalf("after revocation on the same connection: %d %+v", code, body)
	}
}

func TestControllerIdentityTLS_ExpiredCertificateIsRejectedOnAReusedConnection(t *testing.T) {
	tc := newController(t, time.Now())
	b, _ := tc.enroll("laptop")
	if _, err := b.Clients(t.Context()); err != nil {
		t.Fatalf("before expiry: %v", err)
	}

	// TLS verifies with the real clock, so the handshake of the open
	// connection stays valid; only the Authority's clock moves past NotAfter.
	tc.clock.Advance(domain.Defaults().IdentityCertTTL + time.Hour)

	_, err := b.Clients(t.Context())
	wantCode(t, err, domain.CodeUnauthenticated)
	wantCode(t, b.Renew(t.Context()), domain.CodeUnauthenticated)
}

func TestControllerIdentityTLS_LastActiveClientCannotBeRevoked(t *testing.T) {
	tc := newController(t, time.Now())
	owner, err := client.Describe(tc.ownerDir)
	must(t, err)
	wantCode(t, tc.owner.RemoveClient(t.Context(), owner.IdentityID), domain.CodePolicyDenied)
	if _, err := tc.owner.Clients(t.Context()); err != nil {
		t.Fatalf("the owner lost access: %v", err)
	}

	// With a second client, revoking oneself is allowed.
	tc.enroll("laptop")
	must(t, tc.owner.RemoveClient(t.Context(), owner.IdentityID))
	_, err = tc.owner.Clients(t.Context())
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestControllerIdentityTLS_ExpiredClientsDoNotCountAsUsable(t *testing.T) {
	tc := newController(t, time.Now())
	owner, err := client.Describe(tc.ownerDir)
	must(t, err)
	laptop, _ := tc.enroll("laptop")
	laptopID, err := laptop.Describe()
	must(t, err)
	must(t, tc.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE identities SET not_after = ? WHERE id = ?`,
			time.Now().Add(-time.Hour).UnixNano(), string(laptopID.IdentityID))
		return err
	}))
	// The only other client has expired and cannot renew, so the owner is the last usable one.
	wantCode(t, tc.owner.RemoveClient(t.Context(), owner.IdentityID), domain.CodePolicyDenied)
}

func TestControllerIdentityTLS_AuthorizationByRouteAndKind(t *testing.T) {
	tc := newController(t, time.Now())
	clientHTTP := tc.httpClientFor(tc.ownerDir)
	workerHTTP := tc.enrollWorker()
	anonymous := tc.httpClient()
	someClient := "client_01JA0000000000000000000000"

	type probe struct{ method, path, body string }
	clientsOnly := []probe{
		{http.MethodGet, "/v1/clients", ""},
		{http.MethodPost, "/v1/tokens", `{"kind":"client","ttl_seconds":60}`},
		{http.MethodGet, "/v1/status", ""},
		{http.MethodDelete, "/v1/clients/" + someClient, ""},
	}
	unlisted := []probe{
		{http.MethodGet, "/v1/worker/anything", ""},
		{http.MethodGet, "/v1/join", ""}, // listed path, unlisted method
		{http.MethodGet, "/", ""},
		{http.MethodOptions, "/v1/clients", ""},
	}

	for _, p := range append(append([]probe{}, clientsOnly...), unlisted...) {
		code, body, _ := tc.call(anonymous, p.method, p.path, p.body)
		if code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
			t.Errorf("anonymous %s %s: %d %s", p.method, p.path, code, body.Code)
		}
	}
	for _, p := range append(append([]probe{}, clientsOnly...), unlisted...) {
		if p.method == http.MethodGet && p.path == "/v1/clients" {
			continue
		}
		if code, body, _ := tc.call(workerHTTP, p.method, p.path, p.body); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("worker %s %s: %d %s", p.method, p.path, code, body.Code)
		}
	}
	if code, body, _ := tc.call(workerHTTP, http.MethodGet, "/v1/clients", ""); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
		t.Errorf("worker GET /v1/clients: %d %s", code, body.Code)
	}
	for _, p := range unlisted {
		if code, body, _ := tc.call(clientHTTP, p.method, p.path, p.body); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
			t.Errorf("client %s %s: %d %s", p.method, p.path, code, body.Code)
		}
	}
	for _, p := range clientsOnly[:3] {
		if code, _, _ := tc.call(clientHTTP, p.method, p.path, p.body); code >= 300 {
			t.Errorf("client %s %s: %d", p.method, p.path, code)
		}
	}
	// A worker may renew itself: authorised, so the bad CSR is what fails.
	if code, body, _ := tc.call(workerHTTP, http.MethodPost, "/v1/identity/renew", `{"csr":"x"}`); code != http.StatusBadRequest {
		t.Errorf("worker renew: %d %+v", code, body)
	}
	if code, _, _ := tc.call(anonymous, http.MethodPost, "/v1/identity/renew", `{"csr":"x"}`); code != http.StatusUnauthorized {
		t.Errorf("anonymous renew: %d", code)
	}
}

func TestControllerIdentityTLS_RenewRacingRevoke(t *testing.T) {
	tc := newController(t, time.Now())
	for i := range 15 {
		b, dir := tc.enroll("racer")
		info, err := client.Describe(dir)
		must(t, err)

		var wg sync.WaitGroup
		var renewErr error
		wg.Add(2)
		go func() { defer wg.Done(); renewErr = b.Renew(t.Context()) }()
		go func() { defer wg.Done(); must(t, tc.owner.RemoveClient(t.Context(), info.IdentityID)) }()
		wg.Wait()

		// Whatever the order, the identity ends revoked and no certificate of
		// it, old or renewed, authenticates.
		after, err := client.Open(dir)
		must(t, err)
		_, err = after.Clients(t.Context())
		wantCode(t, err, domain.CodeUnauthenticated)
		if renewErr != nil {
			wantCode(t, renewErr, domain.CodeUnauthenticated)
		}
		list, err := tc.owner.Clients(t.Context())
		must(t, err)
		for _, c := range list {
			if c.ID == info.IdentityID && c.RevokedAt == nil {
				t.Fatalf("iteration %d: %s is not revoked", i, c.ID)
			}
		}
	}
}

func TestControllerIdentityTLS_ExpiredTokenIsRejected(t *testing.T) {
	tc := newController(t, time.Now())
	res, err := tc.owner.CreateToken(t.Context(), domain.KindClient, time.Minute)
	must(t, err)
	tc.clock.Advance(2 * time.Minute)

	_, err = client.Connect(t.Context(), res.Invite, "late", t.TempDir()+"/credentials")
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestControllerIdentityTLS_TokenTTLIsBounded(t *testing.T) {
	tc := newController(t, time.Now())
	for _, ttl := range []time.Duration{0, -time.Hour, 7*24*time.Hour + time.Second, 1000 * 24 * time.Hour} {
		_, err := tc.owner.CreateToken(t.Context(), domain.KindClient, ttl)
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	if _, err := tc.owner.CreateToken(t.Context(), domain.KindClient, 7*24*time.Hour); err != nil {
		t.Errorf("the maximum TTL must be accepted: %v", err)
	}
	// A value that overflows time.Duration is refused, not wrapped around.
	code, body, _ := tc.call(tc.httpClientFor(tc.ownerDir), http.MethodPost, "/v1/tokens",
		`{"kind":"client","ttl_seconds":9223372036854775807}`)
	if code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("overflowing ttl: %d %+v", code, body)
	}
	_, err := tc.owner.CreateToken(t.Context(), domain.Kind("owner"), time.Hour)
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestControllerIdentityTLS_AnInvitationIsRedeemedOnlyAsItsKind(t *testing.T) {
	tc := newController(t, time.Now())
	res, err := tc.owner.CreateToken(t.Context(), domain.KindWorker, time.Hour)
	must(t, err)

	// Redeemed as a client it is refused, and the refusal does not spend it.
	for range 2 {
		_, err = client.Connect(t.Context(), res.Invite, "w", t.TempDir()+"/credentials")
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	list, err := tc.owner.Clients(t.Context())
	must(t, err)
	if len(list) != 1 {
		t.Errorf("a refused join created identities: %+v", list)
	}
	dir := t.TempDir() + "/credentials"
	if _, err = client.Connect(t.Context(), res.Invite, "w", dir, client.AsWorker(v1.WorkerSettings{})); err != nil {
		t.Fatalf("as a Worker: %v", err)
	}

	// And the other way round.
	clientInvite, err := tc.owner.CreateToken(t.Context(), domain.KindClient, time.Hour)
	must(t, err)
	_, err = client.Connect(t.Context(), clientInvite.Invite, "m", t.TempDir()+"/credentials", client.AsWorker(v1.WorkerSettings{}))
	wantCode(t, err, domain.CodeInvalidRequest)
	if _, err = client.Connect(t.Context(), clientInvite.Invite, "m", t.TempDir()+"/credentials"); err != nil {
		t.Errorf("a client invitation, after the refusal: %v", err)
	}
	list, err = tc.owner.Clients(t.Context())
	must(t, err)
	if len(list) != 2 { // the owner and the new client; the Worker is not a client
		t.Errorf("clients = %+v", list)
	}
}

func TestControllerIdentityTLS_IssuancePolicy(t *testing.T) {
	tc := newController(t, time.Now())
	anonymous := tc.httpClient()

	join := func(_ *testing.T, token, csr string) (int, v1.ErrorBody, []byte) {
		body, _ := json.Marshal(v1.JoinRequest{Token: token, CSR: csr, Name: "m"})
		return tc.call(anonymous, http.MethodPost, "/v1/join", string(body))
	}
	res, err := tc.owner.CreateToken(t.Context(), domain.KindClient, time.Hour)
	must(t, err)
	inv, err := v1.ParseInvite(res.Invite)
	must(t, err)

	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	t.Run("rejected CSRs leave the token unspent", func(t *testing.T) {
		tampered := csrPEM(t, p256, &x509.CertificateRequest{})
		block, _ := pem.Decode([]byte(tampered))
		block.Bytes[len(block.Bytes)-3] ^= 0xff
		for name, csr := range map[string]string{
			"P-384":            csrPEM(t, p384, &x509.CertificateRequest{}),
			"RSA":              csrPEM(t, rsaKey, &x509.CertificateRequest{}),
			"bad signature":    string(pem.EncodeToMemory(block)),
			"not a CSR":        "hello",
			"wrong PEM header": strings.ReplaceAll(csrPEM(t, p256, &x509.CertificateRequest{}), "CERTIFICATE REQUEST", "CERTIFICATE"),
		} {
			if code, body, _ := join(t, inv.Token, csr); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
				t.Errorf("%s: %d %+v", name, code, body)
			}
		}
	})

	t.Run("the certificate comes from the Controller's template, not the CSR", func(t *testing.T) {
		// A CSR that asks for a CA, SANs and someone else's name.
		basicConstraints, _ := asn1.Marshal(struct{ IsCA bool }{true})
		csr := csrPEM(t, p256, &x509.CertificateRequest{
			Subject:  pkix.Name{CommonName: "client_01JAVICTIM000000000000000"},
			DNSNames: []string{"controller.example"},
			ExtraExtensions: []pkix.Extension{
				{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: basicConstraints},
			},
		})
		code, _, raw := join(t, inv.Token, csr)
		if code != http.StatusCreated {
			t.Fatalf("join: %d %s", code, raw)
		}
		var out v1.CertificateResponse
		must(t, json.Unmarshal(raw, &out))
		block, _ := pem.Decode([]byte(out.Certificate))
		cert, err := x509.ParseCertificate(block.Bytes)
		must(t, err)

		if cert.IsCA || len(cert.DNSNames)+len(cert.IPAddresses)+len(cert.EmailAddresses)+len(cert.URIs) != 0 {
			t.Errorf("certificate copied from the CSR: ca=%v sans=%v", cert.IsCA, cert.DNSNames)
		}
		if cert.Subject.CommonName != out.IdentityID || !strings.HasPrefix(out.IdentityID, "client_") {
			t.Errorf("CN %q, identity %q", cert.Subject.CommonName, out.IdentityID)
		}
		if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || cert.KeyUsage != x509.KeyUsageDigitalSignature {
			t.Errorf("usages: %v %v", cert.ExtKeyUsage, cert.KeyUsage)
		}
		if cert.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() { //nolint:forcetypeassert // the template only issues ECDSA
			t.Error("not P-256")
		}
		if ttl := cert.NotAfter.Sub(cert.NotBefore); ttl < 89*24*time.Hour || ttl > 91*24*time.Hour {
			t.Errorf("lifetime %s", ttl)
		}
		if err := cert.CheckSignatureFrom(tc.ca.Cert); err != nil {
			t.Error(err)
		}
	})

	t.Run("the token is read from the body only", func(t *testing.T) {
		res, err := tc.owner.CreateToken(t.Context(), domain.KindClient, time.Hour)
		must(t, err)
		other, _ := v1.ParseInvite(res.Invite)
		csr := csrPEM(t, p256, &x509.CertificateRequest{})
		u := "https://" + tc.addr + "/v1/join?" + url.Values{"token": {other.Token}}.Encode()
		body, _ := json.Marshal(v1.JoinRequest{CSR: csr})
		if code, _, _ := doRaw(t, anonymous, u, http.MethodPost, string(body), nil); code != http.StatusUnauthorized {
			t.Fatalf("a token in the query was honoured: %d", code)
		}
		if code, _, _ := join(t, other.Token, csr); code != http.StatusCreated {
			t.Fatalf("the token should still be valid: %d", code)
		}
	})

	t.Run("unknown fields and oversized bodies are refused", func(t *testing.T) {
		if code, _, _ := tc.call(anonymous, http.MethodPost, "/v1/join", `{"token":"x","csr":"y","admin":true}`); code != http.StatusBadRequest {
			t.Errorf("unknown field: %d", code)
		}
		if code, _, _ := tc.call(anonymous, http.MethodPost, "/v1/join", `{"token":"`+strings.Repeat("a", 100<<10)+`"}`); code != http.StatusBadRequest {
			t.Errorf("oversized: %d", code)
		}
	})
}

func TestControllerIdentityTLS_AuditHoldsNoSecrets(t *testing.T) {
	tc := newController(t, time.Now())
	res, err := tc.owner.CreateToken(t.Context(), domain.KindClient, time.Hour)
	must(t, err)
	inv, _ := v1.ParseInvite(res.Invite)
	dir := t.TempDir() + "/credentials"
	_, err = client.Connect(t.Context(), res.Invite, "laptop", dir)
	must(t, err)
	b, err := client.Open(dir)
	must(t, err)
	must(t, b.Renew(t.Context()))
	info, err := client.Describe(dir)
	must(t, err)
	must(t, tc.owner.RemoveClient(t.Context(), info.IdentityID))
	must(t, tc.owner.RemoveClient(t.Context(), info.IdentityID)) // idempotent: no second entry

	rows, err := tc.db.Reader().QueryContext(t.Context(), `SELECT action, COALESCE(before,''), COALESCE(after,''), resource_id FROM audit_entries ORDER BY id`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	var actions []string
	for rows.Next() {
		var action, before, after, resource string
		must(t, rows.Scan(&action, &before, &after, &resource))
		actions = append(actions, action)
		if all := action + before + after + resource; strings.Contains(all, inv.Token) || strings.Contains(all, "PRIVATE KEY") {
			t.Errorf("secret in audit entry %s", action)
		}
	}
	must(t, rows.Err())
	want := []string{"identity.joined", "token.created", "identity.joined", "identity.renewed", "identity.revoked"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit actions\n got %v\nwant %v", actions, want)
	}

	var stored int
	must(t, tc.db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM join_tokens WHERE hash = ?`, inv.Token).Scan(&stored))
	if stored != 0 {
		t.Error("the raw token is stored")
	}
}

func TestControllerIdentityTLS_RevokeEdgeCases(t *testing.T) {
	tc := newController(t, time.Now())
	err := tc.owner.RemoveClient(t.Context(), "client_01JA0000000000000000000000")
	wantCode(t, err, domain.CodeNotFound)
	wantCode(t, tc.owner.RemoveClient(t.Context(), "not-an-id"), domain.CodeNotFound)
	wantCode(t, tc.owner.RemoveClient(t.Context(), "worker_01JA0000000000000000000000"), domain.CodeNotFound)
}

func TestControllerIdentityTLS_ServerClosesAConnectionThatStallsMidBody(t *testing.T) {
	tc := newController(t, time.Now(), func(c *api.Config) {
		c.Timeouts = api.Timeouts{Read: 300 * time.Millisecond, Write: 300 * time.Millisecond, Idle: time.Second}
	})
	roots := x509.NewCertPool()
	roots.AddCert(tc.ca.Cert)
	d := tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1"}}
	conn, err := d.DialContext(t.Context(), "tcp", tc.addr)
	must(t, err)
	defer func() { _ = conn.Close() }()

	// The body is announced as 100 bytes and 5 arrive; the client then goes silent.
	_, err = conn.Write([]byte("POST /v1/join HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"tok"))
	must(t, err)

	const patience = 5 * time.Second
	must(t, conn.SetReadDeadline(time.Now().Add(patience)))
	start := time.Now()
	_, err = io.Copy(io.Discard, conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the server still held the connection open after %s", time.Since(start))
	}
}

func TestControllerIdentityTLS_ServerNeedsTLS13(t *testing.T) {
	tc := newController(t, time.Now())
	roots := x509.NewCertPool()
	roots.AddCert(tc.ca.Cert)
	d := tls.Dialer{Config: &tls.Config{ //nolint:gosec // the point is to attempt a downgrade
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "127.0.0.1"}}
	conn, err := d.DialContext(t.Context(), "tcp", tc.addr)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a TLS 1.2 handshake succeeded")
	}
}

// certOf is the certificate stored in a credentials directory.
func certOf(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	creds, err := client.LoadCredentials(dir)
	must(t, err)
	block, _ := pem.Decode(creds.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	must(t, err)
	return cert
}

func newCSR(t *testing.T) string {
	t.Helper()
	_, csr, err := client.GenerateKeyAndCSR()
	must(t, err)
	return csr
}

// loseRenewalResponse renews through the API and throws the response away, as
// if the connection dropped, the process died or the local write failed.
func loseRenewalResponse(t *testing.T, tc *testController, dir string) {
	t.Helper()
	body, err := json.Marshal(v1.RenewRequest{CSR: newCSR(t)})
	must(t, err)
	if code, _, raw := tc.call(tc.httpClientFor(dir), http.MethodPost, "/v1/identity/renew", string(body)); code != http.StatusOK {
		t.Fatalf("renew: %d %s", code, raw)
	}
}

func wantRecoveryHint(t *testing.T, err error) {
	t.Helper()
	wantCode(t, err, domain.CodeUnauthenticated)
	if !strings.Contains(err.Error(), "cf connect --force") {
		t.Errorf("no recovery hint in %q", err)
	}
}

func TestControllerIdentityTLS_RenewNeedsACertificateThatIsValidAtCommit(t *testing.T) {
	t.Run("the presented certificate expires after authentication", func(t *testing.T) {
		tc := newController(t, time.Now())
		presented := certOf(t, tc.ownerDir)
		tc.clock.Advance(domain.Defaults().IdentityCertTTL + time.Hour)

		owner, err := client.Describe(tc.ownerDir)
		must(t, err)
		_, err = tc.authority.Renew(t.Context(), sessionOf(owner.IdentityID, presented), newCSR(t))
		wantCode(t, err, domain.CodeUnauthenticated)
	})

	t.Run("the identity's certificate expired after authentication", func(t *testing.T) {
		tc := newController(t, time.Now())
		owner, err := client.Describe(tc.ownerDir)
		must(t, err)
		must(t, tc.db.WithTx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(t.Context(), `UPDATE identities SET not_after = ? WHERE id = ?`,
				tc.clock.Now().Add(-time.Second).UnixNano(), owner.IdentityID)
			return err
		}))
		_, err = tc.authority.Renew(t.Context(), sessionOf(owner.IdentityID, certOf(t, tc.ownerDir)), newCSR(t))
		wantCode(t, err, domain.CodeUnauthenticated)
	})
}

func TestScenario_ControllerIdentityTLS_RenovacionPorConexionReutilizada(t *testing.T) {
	tc := newController(t, time.Now())
	b, _ := tc.enroll("laptop")
	if _, err := b.Clients(t.Context()); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	tc.clock.Advance(domain.Defaults().IdentityCertTTL + time.Hour)

	_, err := b.Clients(t.Context())
	wantRecoveryHint(t, err)
	wantRecoveryHint(t, b.Renew(t.Context()))
}

func TestControllerIdentityTLS_RevokedClientGetsTheRecoveryHint(t *testing.T) {
	tc := newController(t, time.Now())
	b, dir := tc.enroll("laptop")
	info, err := client.Describe(dir)
	must(t, err)
	must(t, tc.owner.RemoveClient(t.Context(), info.IdentityID))

	_, err = b.Clients(t.Context())
	wantRecoveryHint(t, err)
}

func TestControllerIdentityTLS_ClientKeepsWorkingAfterRenew(t *testing.T) {
	tc := newController(t, time.Now())
	b, _ := tc.enroll("laptop")

	must(t, b.Renew(t.Context()))
	if _, err := b.Clients(t.Context()); err != nil {
		t.Fatalf("after the first renewal: %v", err)
	}
	must(t, b.Renew(t.Context()))
	if _, err := b.Clients(t.Context()); err != nil {
		t.Fatalf("after the second renewal: %v", err)
	}
}

func TestControllerIdentityTLS_ConcurrentRenewsLeaveTheClientUsable(t *testing.T) {
	tc := newController(t, time.Now())
	b, dir := tc.enroll("laptop")

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { must(t, b.Renew(t.Context())) })
	}
	wg.Wait()

	if _, err := b.Clients(t.Context()); err != nil {
		t.Fatalf("shared client: %v", err)
	}
	reopened, err := client.Open(dir)
	must(t, err)
	if _, err := reopened.Clients(t.Context()); err != nil {
		t.Fatalf("stored credentials: %v", err)
	}
}

func TestControllerIdentityTLS_InterruptedRenewalCanBeRetried(t *testing.T) {
	t.Run("the old credentials renew again within the grace", func(t *testing.T) {
		tc := newController(t, time.Now())
		_, dir := tc.enroll("laptop")
		loseRenewalResponse(t, tc, dir) // the stored credentials are still the old ones

		retry, err := client.Open(dir)
		must(t, err)
		must(t, retry.Renew(t.Context()))
		again, err := client.Open(dir)
		must(t, err)
		if _, err := again.Clients(t.Context()); err != nil {
			t.Fatalf("after the retry: %v", err)
		}
	})

	t.Run("the grace only covers renewing", func(t *testing.T) {
		tc := newController(t, time.Now())
		_, dir := tc.enroll("laptop")
		loseRenewalResponse(t, tc, dir)

		old := tc.httpClientFor(dir)
		for _, p := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/clients", ""},
			{http.MethodPost, "/v1/tokens", `{"kind":"client","ttl_seconds":60}`},
		} {
			if code, body, _ := tc.call(old, p.method, p.path, p.body); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
				t.Errorf("%s %s with the superseded certificate: %d %+v", p.method, p.path, code, body)
			}
		}
		stale, err := client.Open(dir)
		must(t, err)
		_, err = stale.Clients(t.Context())
		wantRecoveryHint(t, err)
	})

	t.Run("outside the grace the old credentials are refused", func(t *testing.T) {
		tc := newController(t, time.Now())
		_, dir := tc.enroll("laptop")
		loseRenewalResponse(t, tc, dir)
		tc.clock.Advance(10*time.Minute + time.Second)

		late, err := client.Open(dir)
		must(t, err)
		wantRecoveryHint(t, late.Renew(t.Context()))
	})

	t.Run("a retry does not extend the grace", func(t *testing.T) {
		tc := newController(t, time.Now())
		_, dir := tc.enroll("laptop")
		original := t.TempDir() + "/credentials"
		creds, err := client.LoadCredentials(dir)
		must(t, err)
		must(t, client.SaveCredentials(original, creds))

		loseRenewalResponse(t, tc, dir)
		// Less than the one minute of back-dating: the client checks new certificates against the real clock.
		tc.clock.Advance(50 * time.Second)
		retry, err := client.Open(dir)
		must(t, err)
		must(t, retry.Renew(t.Context()))                // renews from the original certificate, inside its grace
		tc.clock.Advance(9*time.Minute + 40*time.Second) // past 10 minutes after the first renewal

		stale, err := client.Open(original)
		must(t, err)
		wantRecoveryHint(t, stale.Renew(t.Context()))
		current, err := client.Open(dir)
		must(t, err)
		if _, err := current.Clients(t.Context()); err != nil {
			t.Fatalf("the credentials from the retry: %v", err)
		}
	})

	t.Run("revoking ends the grace", func(t *testing.T) {
		tc := newController(t, time.Now())
		_, dir := tc.enroll("laptop")
		info, err := client.Describe(dir)
		must(t, err)
		loseRenewalResponse(t, tc, dir)
		must(t, tc.owner.RemoveClient(t.Context(), info.IdentityID))

		old, err := client.Open(dir)
		must(t, err)
		wantRecoveryHint(t, old.Renew(t.Context()))
	})
}

func TestControllerIdentityTLS_ExpiredClientCanBeRevokedEvenIfOnlyOneIsUsable(t *testing.T) {
	tc := newController(t, time.Now())
	laptop, _ := tc.enroll("laptop")
	laptopID, err := laptop.Describe()
	must(t, err)
	must(t, tc.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE identities SET not_after = ? WHERE id = ?`,
			time.Now().Add(-time.Hour).UnixNano(), laptopID.IdentityID)
		return err
	}))

	must(t, tc.owner.RemoveClient(t.Context(), laptopID.IdentityID))
	if _, err := tc.owner.Clients(t.Context()); err != nil {
		t.Fatalf("the owner lost access: %v", err)
	}
}

func sessionOf(id string, cert *x509.Certificate) pki.Session {
	return pki.Session{Principal: domain.Principal{ID: domain.ID(id), Kind: domain.KindClient}, Certificate: cert}
}

func (tc *testController) sessionFor(dir string) pki.Session {
	tc.t.Helper()
	s, err := tc.authority.Authenticate(tc.t.Context(), certOf(tc.t, dir))
	must(tc.t, err)
	return s
}

// holdWriter occupies the database writer until the returned release is
// called, and waits long enough for a concurrent operation to queue behind it.
func (tc *testController) holdWriter() (release func()) {
	tc.t.Helper()
	held, free, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = tc.db.WithTx(tc.t.Context(), func(*store.Tx) error {
			close(held)
			<-free
			return nil
		})
	}()
	<-held
	return func() { close(free); <-done }
}

// queueBehindWriter runs op while the writer is held, moves the Authority's
// clock by advance once op has had time to queue, then releases the writer.
func (tc *testController) queueBehindWriter(advance time.Duration, op func() error) error {
	tc.t.Helper()
	release := tc.holdWriter()
	result := make(chan error, 1)
	go func() { result <- op() }()
	time.Sleep(200 * time.Millisecond)
	tc.clock.Advance(advance)
	release()
	return <-result
}

func (tc *testController) countRows(query string) (n int) {
	tc.t.Helper()
	must(tc.t, tc.db.WithTx(tc.t.Context(), func(tx *store.Tx) error {
		return tx.QueryRow(tc.t.Context(), query).Scan(&n)
	}))
	return n
}

func TestControllerIdentityTLS_RenewReadsTheClockAfterTakingTheWriter(t *testing.T) {
	tc := newController(t, time.Now())
	session := tc.sessionFor(tc.ownerDir)
	before, err := tc.authority.Clients(t.Context())
	must(t, err)

	err = tc.queueBehindWriter(domain.Defaults().IdentityCertTTL+time.Hour, func() error {
		_, err := tc.authority.Renew(t.Context(), session, newCSR(t))
		return err
	})
	wantCode(t, err, domain.CodeUnauthenticated)

	after, err := tc.authority.Clients(t.Context())
	must(t, err)
	if len(after) != 1 || after[0].CertSerial != before[0].CertSerial {
		t.Errorf("a certificate was issued: %+v -> %+v", before, after)
	}
}

func TestControllerIdentityTLS_JoinReadsTheClockAfterTakingTheWriter(t *testing.T) {
	tc := newController(t, time.Now())
	invite, _, err := tc.authority.CreateToken(t.Context(), tc.sessionFor(tc.ownerDir), domain.KindClient, time.Minute)
	must(t, err)

	err = tc.queueBehindWriter(2*time.Minute, func() error {
		_, err := tc.authority.Join(t.Context(), invite.Token, newCSR(t), "late", nil)
		return err
	})
	wantCode(t, err, domain.CodeUnauthenticated)

	if n := tc.countRows(`SELECT count(*) FROM join_tokens WHERE used_at IS NOT NULL`); n != 0 {
		t.Errorf("%d tokens consumed", n)
	}
	if n := tc.countRows(`SELECT count(*) FROM identities`); n != 1 {
		t.Errorf("%d identities, want only the owner", n)
	}
}

func TestControllerIdentityTLS_ARevokedIssuerCannotCreateInvitations(t *testing.T) {
	tc := newController(t, time.Now())
	_, dir := tc.enroll("laptop")
	laptop := tc.sessionFor(dir) // authenticated before the revocation
	tokens := tc.countRows(`SELECT count(*) FROM join_tokens`)
	must(t, tc.owner.RemoveClient(t.Context(), string(laptop.Principal.ID)))

	_, _, err := tc.authority.CreateToken(t.Context(), laptop, domain.KindClient, time.Hour)
	wantCode(t, err, domain.CodeUnauthenticated)
	if n := tc.countRows(`SELECT count(*) FROM join_tokens`); n != tokens {
		t.Errorf("%d tokens, want %d", n, tokens)
	}
}

func TestControllerIdentityTLS_ARevokedClientCannotRevokeOthers(t *testing.T) {
	tc := newController(t, time.Now())
	_, dir := tc.enroll("laptop")
	laptop := tc.sessionFor(dir)
	owner, err := client.Describe(tc.ownerDir)
	must(t, err)
	must(t, tc.owner.RemoveClient(t.Context(), string(laptop.Principal.ID)))

	wantCode(t, tc.authority.RevokeClient(t.Context(), laptop, domain.ID(owner.IdentityID)), domain.CodeUnauthenticated)
	if n := tc.countRows(`SELECT count(*) FROM identities WHERE revoked_at IS NULL`); n != 1 {
		t.Errorf("%d usable identities, want the owner only", n)
	}
}
