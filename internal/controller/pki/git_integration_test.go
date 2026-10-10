//go:build integration

package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGitTrustsControllerCert is the spike behind ADR 0002 D2: the ECDSA P-256
// CA and leaves we issue must complete a TLS handshake with the git/libcurl of
// the host (macOS locally, Debian/Ubuntu in CI). The server answers 404 to
// everything, so success means "the handshake worked", whatever git then says.
func TestGitTrustsControllerCert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	ca, err := NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, ca.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	serverCert, err := ca.IssueServerCert("127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	url := "https://" + srv.Listener.Addr().String() + "/repo.git"

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "credential.helper="}, args...)...) //nolint:gosec // fixed test arguments
		cmd.Env = append(cleanGitEnv(),
			"HOME="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
		out, _ := cmd.CombinedOutput() // it must fail (404); what matters is how
		return string(out)
	}
	isTLSFailure := func(out string) bool {
		low := strings.ToLower(out)
		for _, marker := range []string{"ssl certificate", "ssl connect", "tls", "certificate verif", "unable to get local issuer", "ssl_error", "schannel", "securetransport"} {
			if strings.Contains(low, marker) {
				return true
			}
		}
		return false
	}

	t.Run("control: without the CA git refuses the server", func(t *testing.T) {
		if out := git("ls-remote", url); !isTLSFailure(out) {
			t.Fatalf("expected a TLS failure without http.sslCAInfo, got:\n%s", out)
		}
	})

	t.Run("with the Controller CA the handshake completes", func(t *testing.T) {
		out := git("-c", "http.sslCAInfo="+caFile, "-c", "http.sslVerify=true", "ls-remote", url)
		if isTLSFailure(out) {
			t.Fatalf("git could not complete the TLS handshake with the issued certificates:\n%s", out)
		}
		if !strings.Contains(out, "404") && !strings.Contains(strings.ToLower(out), "not found") {
			t.Fatalf("git did not reach the HTTP layer:\n%s", out)
		}
	})

	t.Run("git presents a client certificate issued by the Controller", func(t *testing.T) {
		clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "client_X"},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, ca.Cert, &clientKey.PublicKey, ca.Key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, _ := x509.MarshalECPrivateKey(clientKey)
		certFile, keyFile := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
		if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
			t.Fatal(err)
		}

		mtls := httptest.NewUnstartedServer(http.NotFoundHandler())
		pool := x509.NewCertPool()
		pool.AddCert(ca.Cert)
		mtls.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
		mtls.StartTLS()
		t.Cleanup(mtls.Close)

		out := git("-c", "http.sslCAInfo="+caFile, "-c", "http.sslCert="+certFile, "-c", "http.sslKey="+keyFile,
			"ls-remote", "https://"+mtls.Listener.Addr().String()+"/repo.git")
		if isTLSFailure(out) || !strings.Contains(out, "404") && !strings.Contains(strings.ToLower(out), "not found") {
			t.Fatalf("mutual TLS with git failed:\n%s", out)
		}
	})
}

// cleanGitEnv drops the variables that would change what the test measures.
func cleanGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_SSL") || strings.HasPrefix(kv, "GIT_CURL") || strings.HasPrefix(kv, "GIT_TRACE") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
