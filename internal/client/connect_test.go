package client_test

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

// fakeController is a TLS server that records what reaches the HTTP layer.
type fakeController struct {
	srv      *httptest.Server
	requests atomic.Int32
	bodies   chan string
}

func startFake(t *testing.T, chain tls.Certificate, handler http.HandlerFunc) *fakeController {
	t.Helper()
	f := &fakeController{bodies: make(chan string, 8)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.bodies <- string(body)
		handler(w, r)
	}))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{chain}, MinVersion: tls.VersionTLS13}
	f.srv.Config.ErrorLog = nil
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeController) hostPort() (string, string) {
	host, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	return host, port
}

func inviteFor(f *fakeController, ca *pki.CA, token string) string {
	host, port := f.hostPort()
	return v1.Invite{Host: host, Port: port, Token: token, Fingerprint: ca.Fingerprint()}.String()
}

func newCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func serverChain(t *testing.T, ca *pki.CA, host string, at time.Time) tls.Certificate {
	t.Helper()
	c, err := ca.IssueServerCert(host, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rejectJoin(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(v1.ErrorEnvelope{Error: v1.ErrorBody{Code: domain.CodeUnauthenticated, Message: "no"}})
}

func TestScenario_ControllerIdentityTLS_FingerprintMismatchesAbortBeforeSendingTheToken(t *testing.T) {
	const token = "super-secret-token"
	ca := newCA(t)
	other := newCA(t)
	now := time.Now()

	// A leaf signed by `other` that the server dresses up with `ca`'s certificate.
	forged := serverChain(t, other, "127.0.0.1", now)
	forged.Certificate[1] = ca.Cert.Raw

	leaf := serverChain(t, ca, "127.0.0.1", now)
	cases := map[string]tls.Certificate{
		"another CA":                       serverChain(t, other, "127.0.0.1", now),
		"the right CA but a wrong SAN":     serverChain(t, ca, "10.9.9.9", now),
		"the right CA but a wrong DNS SAN": serverChain(t, ca, "controller.invalid", now),
		"an expired leaf":                  serverChain(t, ca, "127.0.0.1", now.Add(-2*365*24*time.Hour)),
		"a chain that does not verify":     forged,
		"no CA at all in the chain":        {Certificate: leaf.Certificate[:1], PrivateKey: leaf.PrivateKey},
	}

	for name, chain := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFake(t, chain, rejectJoin)
			dir := filepath.Join(t.TempDir(), "credentials")

			_, err := client.Connect(t.Context(), inviteFor(f, ca, token), "laptop", dir)

			var de *domain.Error
			if !asDomain(err, &de) || de.Code != domain.CodeUnauthenticated {
				t.Fatalf("err = %v, want CF_UNAUTHENTICATED", err)
			}
			if !strings.Contains(de.Message, "does not match the invitation") {
				t.Errorf("rejected by the wrong check: %q", de.Message)
			}
			if n := f.requests.Load(); n != 0 {
				t.Fatalf("%d request(s) reached the server; the token may have leaked", n)
			}
			if _, statErr := os.Lstat(dir); statErr == nil {
				t.Error("credentials were written")
			}
		})
	}

	t.Run("control: the right server does receive the token", func(t *testing.T) {
		f := startFake(t, serverChain(t, ca, "127.0.0.1", now), rejectJoin)
		_, err := client.Connect(t.Context(), inviteFor(f, ca, token), "laptop", filepath.Join(t.TempDir(), "credentials"))
		var de *domain.Error
		if !asDomain(err, &de) || de.Code != domain.CodeUnauthenticated {
			t.Fatalf("err = %v", err)
		}
		if body := <-f.bodies; !strings.Contains(body, token) {
			t.Fatalf("body %q lacks the token: the harness would not have noticed a leak", body)
		}
	})
}

func TestConnect_TokenNeverTravelsInTheURL(t *testing.T) {
	ca := newCA(t)
	var rawURL atomic.Value
	f := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), func(w http.ResponseWriter, r *http.Request) {
		rawURL.Store(r.URL.String())
		rejectJoin(w, r)
	})
	_, _ = client.Connect(t.Context(), inviteFor(f, ca, "tok-123"), "m", filepath.Join(t.TempDir(), "credentials"))
	if got, _ := rawURL.Load().(string); got != "/v1/join" {
		t.Fatalf("request URL = %q", got)
	}
}

func TestConnect_DoesNotFollowRedirects(t *testing.T) {
	ca := newCA(t)
	target := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), rejectJoin)
	f := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+"/v1/join", http.StatusTemporaryRedirect)
	})

	_, err := client.Connect(t.Context(), inviteFor(f, ca, "tok"), "m", filepath.Join(t.TempDir(), "credentials"))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if n := target.requests.Load(); n != 0 {
		t.Fatalf("the redirect was followed (%d requests)", n)
	}
}

func TestConnect_UnreachableController(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()

	ca := newCA(t)
	inv := v1.Invite{Host: host, Port: port, Token: "t", Fingerprint: ca.Fingerprint()}.String()
	_, err = client.Connect(t.Context(), inv, "m", filepath.Join(t.TempDir(), "credentials"))
	var de *domain.Error
	if !asDomain(err, &de) || de.Code != domain.CodeControllerUnreachable {
		t.Fatalf("err = %v", err)
	}
	if domain.ExitCode(de.Code) != 6 {
		t.Errorf("exit %d", domain.ExitCode(de.Code))
	}
}

func TestConnect_RefusesAMalformedInvitationAndAnUnsafeDestination(t *testing.T) {
	_, err := client.Connect(t.Context(), "https://example.com", "m", filepath.Join(t.TempDir(), "credentials"))
	var de *domain.Error
	if !asDomain(err, &de) || de.Code != v1.CodeInvalidRequest {
		t.Fatalf("err = %v", err)
	}

	// A symlinked destination is refused before the token is spent.
	ca := newCA(t)
	f := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), rejectJoin)
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "elsewhere"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "credentials")
	if err := os.Symlink(filepath.Join(base, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	_, err = client.Connect(t.Context(), inviteFor(f, ca, "tok"), "m", link)
	if !isUnsafe(err) {
		t.Fatalf("err = %v", err)
	}
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("the join was sent to an unsafe destination (%d requests)", n)
	}
}

func TestClient_RejectsAControllerWithAnotherCA(t *testing.T) {
	ca, other := newCA(t), newCA(t)
	dir := filepath.Join(t.TempDir(), "credentials")
	creds := makeBundle(t, ca, "127.0.0.1:1") // address replaced below
	f := startFake(t, serverChain(t, other, "127.0.0.1", time.Now()), rejectJoin)
	host, port := f.hostPort()
	creds.Controller = net.JoinHostPort(host, port)
	if err := client.SaveCredentials(dir, creds); err != nil {
		t.Fatal(err)
	}
	c, err := client.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Clients(t.Context())
	var de *domain.Error
	if !asDomain(err, &de) || de.Code != domain.CodeUnauthenticated {
		t.Fatalf("err = %v", err)
	}
	if f.requests.Load() != 0 {
		t.Error("a request reached a Controller that is not ours")
	}
}
