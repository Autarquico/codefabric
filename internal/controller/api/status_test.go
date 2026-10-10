package api_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/domain"
)

// storeStub is the real store with a Health the test controls.
type storeStub struct {
	api.Store
	health func(context.Context) error
}

func (s storeStub) Health(ctx context.Context) error { return s.health(ctx) }

func withHealth(health func(context.Context) error) func(*api.Config) {
	return func(c *api.Config) { c.Store = storeStub{Store: c.Store, health: health} }
}

func TestApiSdkCliStatus_StatusReportsIdentityVersionAndUptimeFromTheInjectedClock(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	tc := newController(t, start)

	st, err := tc.owner.Status(t.Context())
	must(t, err)
	if st.ControllerID != tc.db.ControllerID() || st.ControllerID == "" {
		t.Errorf("controller id %q, want %q", st.ControllerID, tc.db.ControllerID())
	}
	if st.Version != "test" || st.Commit != "abc123" {
		t.Errorf("build %q/%q", st.Version, st.Commit)
	}
	if !st.StartedAt.Equal(start) || st.UptimeSeconds != 0 {
		t.Errorf("started %s uptime %d", st.StartedAt, st.UptimeSeconds)
	}
	if !st.Database.OK || st.Database.Error != "" {
		t.Errorf("database %+v", st.Database)
	}

	tc.clock.Advance(90*time.Minute + 30*time.Second)
	st, err = tc.owner.Status(t.Context())
	must(t, err)
	if want := int64(90*60 + 30); st.UptimeSeconds != want {
		t.Errorf("uptime %d, want %d", st.UptimeSeconds, want)
	}
}

func TestApiSdkCliStatus_AnUnhealthyDatabaseIsReportedWithoutLeakingTheCause(t *testing.T) {
	cause := errors.New("quick_check failed: /var/lib/codefabric/controller.db: SELECT secret FROM identities")
	tc := newController(t, time.Now(), withHealth(func(context.Context) error { return cause }))

	code, _, raw := tc.call(tc.httpClientFor(tc.ownerDir), http.MethodGet, "/v1/status", "")
	if code != http.StatusOK {
		t.Fatalf("status %d: an unhealthy database is a result, not a failed request", code)
	}
	for _, leak := range []string{"/var/lib", "SELECT", "quick_check"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("response leaks %q: %s", leak, raw)
		}
	}
	st, err := tc.owner.Status(t.Context())
	must(t, err)
	if st.Database.OK || st.Database.Error != "database check failed" {
		t.Errorf("database %+v", st.Database)
	}
	if !strings.Contains(tc.logs.String(), "SELECT secret") {
		t.Errorf("the detail should reach the server log:\n%s", tc.logs)
	}
}

func TestApiSdkCliStatus_TheHealthCheckHasATimeout(t *testing.T) {
	var budget time.Duration
	tc := newController(t, time.Now(), withHealth(func(ctx context.Context) error {
		deadline, _ := ctx.Deadline()
		budget = time.Until(deadline)
		<-ctx.Done()
		return ctx.Err()
	}), func(c *api.Config) { c.Timeouts.Health = 50 * time.Millisecond })
	start := time.Now()
	st, err := tc.owner.Status(t.Context())
	must(t, err)
	if budget <= 0 || budget > 50*time.Millisecond {
		t.Errorf("the health check got a %s deadline, want the configured 50ms", budget)
	}
	if st.Database.OK || time.Since(start) > 5*time.Second {
		t.Errorf("database %+v after %s", st.Database, time.Since(start))
	}
}

func TestApiSdkCliStatus_ControllerOverrideKeepsThePinnedTrustAndTheStoredBundle(t *testing.T) {
	tc := newController(t, time.Now())
	other := newController(t, time.Now()) // a Controller with a different CA
	_, port, _ := net.SplitHostPort(tc.addr)
	before, err := client.LoadCredentials(tc.ownerDir)
	must(t, err)

	t.Run("same host", func(t *testing.T) {
		c, err := openOverride(tc.ownerDir, tc.addr)
		must(t, err)
		if _, err := c.Status(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a host the certificate does not cover", func(t *testing.T) {
		c, err := openOverride(tc.ownerDir, "localhost:"+port)
		must(t, err)
		_, err = c.Status(t.Context())
		wantCode(t, err, domain.CodeControllerUnreachable)
		if !strings.Contains(err.Error(), "--advertise") {
			t.Errorf("no hint about --advertise: %v", err)
		}
		wantLocalCause(t, err)
	})
	t.Run("a Controller of another CA", func(t *testing.T) {
		c, err := openOverride(tc.ownerDir, other.addr)
		must(t, err)
		_, err = c.Status(t.Context())
		wantCode(t, err, domain.CodeUnauthenticated)
		wantLocalCause(t, err)
	})
	for _, bad := range []string{
		"no-port-here", ":7443", "host:", "host:0", "host:65536", "host:+80", "host:http",
		"host:7443/path", "user@host:7443", "host:7443?x=1", "host:7443#frag", "https://host:7443",
	} {
		t.Run("not an address: "+bad, func(t *testing.T) {
			_, err := openOverride(tc.ownerDir, bad)
			wantCode(t, err, domain.CodeInvalidRequest)
		})
	}
	for _, good := range []string{"example.com:1", "127.0.0.1:65535", "[::1]:7443"} {
		t.Run("an address: "+good, func(t *testing.T) {
			if _, err := openOverride(tc.ownerDir, good); err != nil {
				t.Fatal(err)
			}
		})
	}

	after, err := client.LoadCredentials(tc.ownerDir)
	must(t, err)
	if string(before.CertPEM) != string(after.CertPEM) || before.Controller != after.Controller || before.Controller != tc.addr {
		t.Errorf("the stored bundle changed: %q -> %q", before.Controller, after.Controller)
	}
}

func openOverride(dir, addr string) (*client.Client, error) {
	return client.Open(dir, client.WithController(addr))
}

func TestApiSdkCliStatus_ConcurrentStatusCallsShareOneHealthCheckForAFewSeconds(t *testing.T) {
	var checks atomic.Int64
	tc := newController(t, time.Now(), withHealth(func(context.Context) error {
		checks.Add(1)
		time.Sleep(100 * time.Millisecond) // long enough for the others to arrive
		return nil
	}))

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			st, err := tc.owner.Status(t.Context())
			if err != nil || !st.Database.OK {
				t.Errorf("status %+v: %v", st, err)
			}
		})
	}
	wg.Wait()
	if got := checks.Load(); got != 1 {
		t.Fatalf("%d concurrent calls ran %d health checks, want 1", 20, got)
	}

	tc.clock.Advance(4 * time.Second)
	_, err := tc.owner.Status(t.Context())
	must(t, err)
	if got := checks.Load(); got != 1 {
		t.Errorf("a call within the cache window ran a check: %d", got)
	}

	tc.clock.Advance(2 * time.Second)
	_, err = tc.owner.Status(t.Context())
	must(t, err)
	if got := checks.Load(); got != 2 {
		t.Errorf("a call after the cache window ran %d checks in total, want 2", got)
	}
}

func TestApiSdkCliStatus_AResponseDeclaresItsLengthEvenToAnHTTP10Peer(t *testing.T) {
	healthy := newController(t, time.Now())
	failing := newControllerWithRoutes(t, time.Now(), failureRoutes)

	for name, tc := range map[string]struct {
		ctl    *testController
		path   string
		status int
	}{
		"2xx":   {healthy, "/v1/status", http.StatusOK},
		"error": {failing, "/v1/test/error", http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			got := getHTTP10(t, tc.ctl, tc.path)
			if got.status != tc.status {
				t.Fatalf("status %d, want %d", got.status, tc.status)
			}
			length, err := strconv.Atoi(got.header.Get("Content-Length"))
			if err != nil {
				t.Fatalf("no Content-Length: %v", got.header)
			}
			if len(got.body) != length {
				t.Errorf("declared %d bytes, got %d", length, len(got.body))
			}
		})
	}
}

// getHTTP10 sends an HTTP/1.0 request over mutual TLS as the owner.
func getHTTP10(t *testing.T, tc *testController, path string) reply {
	t.Helper()
	creds, err := client.LoadCredentials(tc.ownerDir)
	must(t, err)
	pair, err := tls.X509KeyPair(creds.CertPEM, creds.KeyPEM)
	must(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(tc.ca.Cert)
	conn, err := (&tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair},
	}}).DialContext(t.Context(), "tcp", tc.addr)
	must(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Write([]byte("GET " + path + " HTTP/1.0\r\n\r\n"))
	must(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	must(t, err)
	return reply{resp.StatusCode, resp.Header, body}
}

// wantLocalCause checks a client-side TLS failure keeps its cause for --debug.
func wantLocalCause(t *testing.T, err error) {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Details["cause"] == nil {
		t.Errorf("the local cause is missing for --debug: %#v", err)
	}
}
