package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/pki"
)

func newTestCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// storeBundle saves a bundle signed by ca into dir, pointing at controller.
func storeBundle(t *testing.T, dir string, ca *pki.CA, controller string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "client_X"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	err = client.SaveCredentials(dir, client.Credentials{
		Controller: controller,
		CertPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:     pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CAPEM:      ca.CertPEM(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// statusServer answers /v1/status with a certificate issued by ca and returns its host:port.
func statusServer(t *testing.T, ca *pki.CA) string {
	t.Helper()
	chain, err := ca.IssueServerCert("127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(v1.Status{ControllerID: "ctl"})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{chain}, MinVersion: tls.VersionTLS13}
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestScenario_BackupRestore_RebindCommandsRefuseADestinationWithAnotherCA(t *testing.T) {
	cases := map[string]struct {
		args []string
		dir  func(config.Paths) string
	}{
		"cf connect --rebind":      {[]string{"connect", "--rebind"}, config.Paths.CredentialsDir},
		"cf worker set-controller": {[]string{"worker", "set-controller"}, config.Paths.WorkerCredentialsDir},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			isolatedMachine(t)
			paths, err := config.ResolveFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			ca := newTestCA(t)
			dir := tc.dir(paths)
			storeBundle(t, dir, ca, "127.0.0.1:1")
			file := filepath.Join(dir, "controller")
			before, _ := os.ReadFile(file)

			code, _, errOut := run(t, append(tc.args, statusServer(t, newTestCA(t)))...)
			if code == 0 {
				t.Fatal("a destination with another CA was accepted")
			}
			if !strings.Contains(errOut, "unchanged") {
				t.Errorf("the error is not actionable: %q", errOut)
			}
			if after, _ := os.ReadFile(file); string(after) != string(before) {
				t.Errorf("controller file changed: %q -> %q", before, after)
			}
		})
	}
}

func TestScenario_BackupRestore_RebindCommandsChangeOnlyTheAddress(t *testing.T) {
	cases := map[string]struct {
		args []string
		dir  func(config.Paths) string
	}{
		"cf connect --rebind":      {[]string{"connect", "--rebind"}, config.Paths.CredentialsDir},
		"cf worker set-controller": {[]string{"worker", "set-controller"}, config.Paths.WorkerCredentialsDir},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			isolatedMachine(t)
			paths, err := config.ResolveFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			ca := newTestCA(t)
			dir := tc.dir(paths)
			storeBundle(t, dir, ca, "127.0.0.1:1")
			cert, _ := os.ReadFile(filepath.Join(dir, "cert.pem"))
			address := statusServer(t, ca)

			if code, out, errOut := run(t, append(tc.args, address)...); code != 0 {
				t.Fatalf("exit %d: %q %q", code, out, errOut)
			}
			got, err := client.LoadCredentials(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got.Controller != address {
				t.Errorf("Controller = %q, want %q", got.Controller, address)
			}
			if after, _ := os.ReadFile(filepath.Join(dir, "cert.pem")); string(after) != string(cert) {
				t.Error("the certificate changed")
			}
		})
	}
}

func TestScenario_BackupRestore_RebindTakesNoInvitation(t *testing.T) {
	isolatedMachine(t)
	if code, _, _ := run(t, "connect", "--rebind", "127.0.0.1:1", "some-invitation"); code != 2 {
		t.Errorf("exit %d, want a usage error", code)
	}
}
