package api_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
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
	tc := newController(t, time.Now(), withHealth(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}))
	start := time.Now()
	st, err := tc.owner.Status(t.Context())
	must(t, err)
	if st.Database.OK || time.Since(start) > 10*time.Second {
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
	})
	t.Run("a Controller of another CA", func(t *testing.T) {
		c, err := openOverride(tc.ownerDir, other.addr)
		must(t, err)
		_, err = c.Status(t.Context())
		wantCode(t, err, domain.CodeUnauthenticated)
	})
	t.Run("not an address", func(t *testing.T) {
		_, err := openOverride(tc.ownerDir, "no-port-here")
		wantCode(t, err, domain.CodeInvalidRequest)
	})

	after, err := client.LoadCredentials(tc.ownerDir)
	must(t, err)
	if string(before.CertPEM) != string(after.CertPEM) || before.Controller != after.Controller || before.Controller != tc.addr {
		t.Errorf("the stored bundle changed: %q -> %q", before.Controller, after.Controller)
	}
}

func openOverride(dir, addr string) (*client.Client, error) {
	return client.Open(dir, client.WithController(addr))
}
