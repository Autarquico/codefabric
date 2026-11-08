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
func savedBundle(t *testing.T, ca interface{ CertPEM() []byte }, makeFor func() client.Credentials) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := client.SaveCredentials(dir, makeFor()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRebind_ADestinationWithAnotherCAIsRejectedAndTheBundleIsUntouched(t *testing.T) {
	ca, other := newCA(t), newCA(t)
	dir := savedBundle(t, ca, func() client.Credentials { return makeBundle(t, ca, "127.0.0.1:1") })
	f := startFake(t, serverChain(t, other, "127.0.0.1", time.Now()), answerStatus)
	host, port := f.hostPort()
	file := filepath.Join(dir, "controller")
	before, _ := os.ReadFile(file)

	_, err := client.Rebind(context.Background(), dir, net.JoinHostPort(host, port))
	if err == nil {
		t.Fatal("a Controller of another CA was accepted")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("controller file changed: %q -> %q", before, after)
	}
	if f.requests.Load() != 0 {
		t.Error("a request reached a Controller that is not ours")
	}
}

func TestRebind_AnUnreachableDestinationWritesNothing(t *testing.T) {
	ca := newCA(t)
	dir := savedBundle(t, ca, func() client.Credentials { return makeBundle(t, ca, "127.0.0.1:1") })
	if _, err := client.Rebind(context.Background(), dir, "127.0.0.1:2"); err == nil {
		t.Fatal("an unreachable destination was accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "controller")); string(got) != "127.0.0.1:1\n" {
		t.Errorf("controller file = %q", got)
	}
}

func TestRebind_AGoodDestinationChangesOnlyTheAddress(t *testing.T) {
	ca := newCA(t)
	creds := makeBundle(t, ca, "127.0.0.1:1")
	dir := savedBundle(t, ca, func() client.Credentials { return creds })
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
