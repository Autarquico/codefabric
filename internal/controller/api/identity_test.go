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
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
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
		{http.MethodDelete, "/v1/clients/" + someClient, ""},
	}
	unlisted := []probe{
		{http.MethodGet, "/v1/status", ""},
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
	for _, p := range clientsOnly[:2] {
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
	_, err := tc.owner.CreateToken(t.Context(), domain.Kind("owner"), time.Hour)
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestControllerIdentityTLS_WorkerTokenIsNotRedeemableYet(t *testing.T) {
	tc := newController(t, time.Now())
	res, err := tc.owner.CreateToken(t.Context(), domain.KindWorker, time.Hour)
	must(t, err)

	for range 2 { // the second attempt shows the first did not spend the token
		_, err = client.Connect(t.Context(), res.Invite, "w", t.TempDir()+"/credentials")
		wantCode(t, err, domain.CodeNotImplemented)
	}
	if got := domain.ExitCode(domain.CodeNotImplemented); got != 8 {
		t.Errorf("exit code %d", got)
	}
	list, err := tc.owner.Clients(t.Context())
	must(t, err)
	if len(list) != 1 {
		t.Errorf("a worker join created identities: %+v", list)
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
