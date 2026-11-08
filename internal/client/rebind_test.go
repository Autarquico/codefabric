package client_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

func answerStatus(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(v1.Status{ControllerID: "ctl", Version: "test"})
}

// savedBundle stores a bundle pointing at an address nobody listens on and returns its directory.
func savedBundle(t *testing.T, makeFor func() client.Credentials) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := client.SaveCredentials(dir, makeFor()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRebind_ADestinationWithAnotherCAIsRejectedAndTheBundleIsUntouched(t *testing.T) {
	ca, other := newCA(t), newCA(t)
	dir := savedBundle(t, func() client.Credentials { return makeBundle(t, ca, "127.0.0.1:1") })
	f := startFake(t, serverChain(t, other, "127.0.0.1", time.Now()), answerStatus)
	host, port := f.hostPort()
	file := filepath.Join(dir, "controller")
	before, _ := os.ReadFile(filepath.Clean(file))

	_, err := client.Rebind(context.Background(), dir, net.JoinHostPort(host, port))
	if err == nil {
		t.Fatal("a Controller of another CA was accepted")
	}
	if after, _ := os.ReadFile(filepath.Clean(file)); string(after) != string(before) {
		t.Errorf("controller file changed: %q -> %q", before, after)
	}
	if f.requests.Load() != 0 {
		t.Error("a request reached a Controller that is not ours")
	}
}

func TestRebind_AnUnreachableDestinationWritesNothing(t *testing.T) {
	ca := newCA(t)
	dir := savedBundle(t, func() client.Credentials { return makeBundle(t, ca, "127.0.0.1:1") })
	if _, err := client.Rebind(context.Background(), dir, "127.0.0.1:2"); err == nil {
		t.Fatal("an unreachable destination was accepted")
	}
	if got, _ := os.ReadFile(filepath.Clean(filepath.Join(dir, "controller"))); string(got) != "127.0.0.1:1\n" {
		t.Errorf("controller file = %q", got)
	}
}

func TestRebind_AGoodDestinationChangesOnlyTheAddress(t *testing.T) {
	ca := newCA(t)
	creds := makeBundle(t, ca, "127.0.0.1:1")
	dir := savedBundle(t, func() client.Credentials { return creds })
	f := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), answerStatus)
	host, port := f.hostPort()
	address := net.JoinHostPort(host, port)

	info, err := client.Rebind(context.Background(), dir, address)
	if err != nil {
		t.Fatal(err)
	}
	if info.Controller != address {
		t.Errorf("Controller = %q, want %q", info.Controller, address)
	}
	got, err := client.LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := creds
	want.Controller = address
	if got.Controller != want.Controller || string(got.CertPEM) != string(want.CertPEM) ||
		string(got.KeyPEM) != string(want.KeyPEM) || string(got.CAPEM) != string(want.CAPEM) {
		t.Errorf("more than the address changed")
	}
	if m := mode(t, filepath.Join(dir, "controller")); m != 0o600 {
		t.Errorf("controller file mode = %04o, want 0600", m)
	}
}

// workerRoutes answers like a Controller does to a Worker: its own orders, and
// CF_POLICY_DENIED for /v1/status.
func workerRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/worker/assignments" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"CF_POLICY_DENIED","message":"this identity may not call this endpoint"}}`))
		return
	}
	_ = json.NewEncoder(w).Encode(v1.AssignmentsResponse{Commands: []v1.WorkerCommand{}})
}

func TestRebindWorker_AsksForTheOrdersOfTheWorkerNotForTheStatus(t *testing.T) {
	ca := newCA(t)
	dir := savedBundle(t, func() client.Credentials { return makeBundle(t, ca, "127.0.0.1:1") })
	f := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), workerRoutes)
	host, port := f.hostPort()
	address := net.JoinHostPort(host, port)

	if _, err := client.Rebind(context.Background(), dir, address); err == nil {
		t.Fatal("the status probe passed against a Controller that denies it")
	}
	if _, err := client.RebindWorker(context.Background(), dir, address); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Clean(filepath.Join(dir, "controller"))); string(got) != address+"\n" {
		t.Errorf("controller file = %q, want %q", got, address)
	}
}

func TestRebindWorker_ARejectedIdentityWritesNothing(t *testing.T) {
	ca := newCA(t)
	dir := savedBundle(t, func() client.Credentials { return makeBundle(t, ca, "127.0.0.1:1") })
	f := startFake(t, serverChain(t, ca, "127.0.0.1", time.Now()), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"CF_UNAUTHENTICATED","message":"unknown identity"}}`))
	})
	host, port := f.hostPort()
	if _, err := client.RebindWorker(context.Background(), dir, net.JoinHostPort(host, port)); err == nil {
		t.Fatal("a 401 was accepted")
	}
	if got, _ := os.ReadFile(filepath.Clean(filepath.Join(dir, "controller"))); string(got) != "127.0.0.1:1\n" {
		t.Errorf("controller file = %q", got)
	}
}
