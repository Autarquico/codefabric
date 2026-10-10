package daemon_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func newPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	return config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")}
}

func initController(t *testing.T, p config.Paths) (daemon.InitResult, string) {
	t.Helper()
	addr := freeAddr(t)
	res, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: addr, Listen: addr, OwnerName: "owner-mac"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return res, addr
}

func start(t *testing.T, p config.Paths) *daemon.Controller {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	c, err := daemon.Start(ctx, daemon.Options{Paths: p})
	if err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := c.Wait(); err != nil {
			t.Errorf("Wait: %v", err)
		}
	})
	return c
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return info.Mode().Perm()
}

func digest(t *testing.T, paths ...string) [][32]byte {
	t.Helper()
	var out [][32]byte
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // test paths
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sha256.Sum256(raw))
	}
	return out
}

func TestScenario_ControllerIdentityTLS_Init(t *testing.T) {
	p := newPaths(t)
	res, _ := initController(t, p)

	// CA, server certificate, DB, config.
	pki := p.PKIDir()
	for file, want := range map[string]os.FileMode{
		filepath.Join(pki, "ca.key"):     0o600,
		filepath.Join(pki, "ca.pem"):     0o600,
		filepath.Join(pki, "server.key"): 0o600,
		filepath.Join(pki, "server.pem"): 0o600,
		p.ControllerConfigFile():         0o600,
	} {
		if got := mode(t, file); got != want {
			t.Errorf("%s mode %04o, want %04o", file, got, want)
		}
	}
	if _, err := os.Stat(p.DatabaseFile()); err != nil {
		t.Errorf("database: %v", err)
	}
	// Owner bundle, 0600 inside a 0700 directory.
	if got := mode(t, p.CredentialsDir()); got != 0o700 {
		t.Errorf("credentials dir %04o", got)
	}
	for _, f := range []string{"cert.pem", "key.pem", "ca.pem", "controller"} {
		if got := mode(t, filepath.Join(p.CredentialsDir(), f)); got != 0o600 {
			t.Errorf("%s mode %04o", f, got)
		}
	}
	info, err := client.Describe(p.CredentialsDir())
	if err != nil || info.IdentityID != string(res.OwnerID) {
		t.Errorf("owner bundle: %+v, %v", info, err)
	}
	// A usable invitation.
	_, err = v1.ParseInvite(res.Invite)
	if err != nil || !res.InviteUntil.After(time.Now()) {
		t.Errorf("invitation %q: %v until %s", res.Invite, err, res.InviteUntil)
	}
	if kind, _, err := domain.ParseID(string(res.OwnerID)); err != nil || kind != domain.KindClient {
		t.Errorf("owner id %q", res.OwnerID)
	}

	// Repeating it overwrites nothing and says why.
	files := []string{
		filepath.Join(pki, "ca.key"), filepath.Join(pki, "ca.pem"), filepath.Join(pki, "server.pem"),
		p.ControllerConfigFile(), filepath.Join(p.CredentialsDir(), "key.pem"),
	}
	before := digest(t, files...)
	_, err = daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: freeAddr(t)})
	if !errors.Is(err, daemon.ErrAlreadyInitialized) {
		t.Fatalf("second init: %v", err)
	}
	if after := digest(t, files...); len(after) != len(before) {
		t.Fatal("digest length")
	} else {
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("%s was overwritten", files[i])
			}
		}
	}
}

func TestInit_RefusesToOverwriteExistingCredentialsAndCleansUpOnFailure(t *testing.T) {
	p := newPaths(t)
	if err := os.MkdirAll(p.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"})
	if !errors.Is(err, daemon.ErrAlreadyInitialized) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(p.DatabaseFile()); statErr == nil {
		t.Error("init created a database before refusing")
	}

	p = newPaths(t)
	for _, bad := range []string{"", "nohost", ":7443", "h:0", "h:99999", "h:port"} {
		if _, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: bad}); err == nil {
			t.Errorf("--advertise %q accepted", bad)
		}
	}
	if _, statErr := os.Stat(p.DataDir); statErr == nil {
		t.Error("a rejected init left state behind")
	}

	// A failure midway (the owner bundle cannot be published) leaves nothing,
	// so the operator can simply retry.
	p = newPaths(t)
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.ConfigDir, 0o500); err != nil { //nolint:gosec // a read-only directory is the failure under test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.ConfigDir, 0o700) }) //nolint:gosec // restoring the test directory
	if _, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"}); err == nil {
		t.Skip("running as a user that ignores directory modes")
	}
	_ = os.Chmod(p.ConfigDir, 0o700) //nolint:gosec // restoring the test directory
	if _, err := os.Stat(p.DatabaseFile()); err == nil {
		t.Error("a failed init left its database behind")
	}
	if _, err := os.Stat(p.PKIDir()); err == nil {
		t.Error("a failed init left its CA behind")
	}
	if _, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"}); err != nil {
		t.Errorf("retry after a failed init: %v", err)
	}
}

func TestStart_NeedsAnInitializedController(t *testing.T) {
	_, err := daemon.Start(t.Context(), daemon.Options{Paths: newPaths(t)})
	if !errors.Is(err, config.ErrNotInitialized) {
		t.Fatalf("err = %v", err)
	}
}

func TestScenario_ControllerIdentityTLS_Fingerprint(t *testing.T) {
	genuine, impostor := newPaths(t), newPaths(t)
	realRes, _ := initController(t, genuine)
	impostorRes, _ := initController(t, impostor)
	start(t, genuine)
	start(t, impostor)

	// The invitation of the real Controller, but pointed at the impostor's
	// address and carrying the real fingerprint: the impostor has another CA.
	realInv, _ := v1.ParseInvite(realRes.Invite)
	impostorInv, _ := v1.ParseInvite(impostorRes.Invite)
	forged := v1.Invite{Host: impostorInv.Host, Port: impostorInv.Port, Token: realInv.Token, Fingerprint: realInv.Fingerprint}

	dir := filepath.Join(t.TempDir(), "credentials")
	_, err := client.Connect(t.Context(), forged.String(), "laptop", dir)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeUnauthenticated {
		t.Fatalf("err = %v, want CF_UNAUTHENTICATED", err)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Error("credentials were saved")
	}

	// The token was never sent anywhere: the real Controller still honours it.
	if _, err := client.Connect(t.Context(), realRes.Invite, "laptop", dir); err != nil {
		t.Fatalf("the real invitation no longer works: %v", err)
	}
	// And it was not delivered to the impostor either: its own token is intact.
	if _, err := client.Connect(t.Context(), impostorRes.Invite, "other", filepath.Join(t.TempDir(), "credentials")); err != nil {
		t.Fatalf("impostor's own invitation: %v", err)
	}
}

func TestScenario_ControllerIdentityTLS_CanjeAtomico(t *testing.T) {
	p := newPaths(t)
	res, _ := initController(t, p)
	start(t, p)

	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = client.Connect(t.Context(), res.Invite, "racer", filepath.Join(t.TempDir(), "credentials"))
		}()
	}
	wg.Wait()

	var winners int
	for _, err := range errs {
		var de *domain.Error
		switch {
		case err == nil:
			winners++
		case errors.As(err, &de) && de.Code == domain.CodeUnauthenticated:
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d redemptions succeeded, want exactly 1", winners)
	}

	owner, err := client.Open(p.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	clients, err := owner.Clients(t.Context())
	if err != nil || len(clients) != 2 { // the owner and the single winner
		t.Fatalf("clients = %d, %v", len(clients), err)
	}
}

func TestInit_ConcurrentRunsLeaveTheWinnerIntact(t *testing.T) {
	for range 5 {
		p := newPaths(t)
		addr := freeAddr(t)
		const racers = 2
		errs := make([]error, racers)
		var wg sync.WaitGroup
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: addr, Listen: addr, OwnerName: "racer"})
			}()
		}
		wg.Wait()

		var winners int
		for _, err := range errs {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, daemon.ErrInitInProgress), errors.Is(err, daemon.ErrAlreadyInitialized):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if winners != 1 {
			t.Fatalf("%d inits succeeded, want exactly 1 (%v)", winners, errs)
		}

		// Whatever the loser did, the winner's Controller still works.
		start(t, p)
		owner, err := client.Open(p.CredentialsDir())
		if err != nil {
			t.Fatalf("owner credentials: %v", err)
		}
		if _, err := owner.Clients(t.Context()); err != nil {
			t.Fatalf("owner cannot use the Controller: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(p.DataDir, ".init.lock")); err == nil {
			t.Error("the lock was left behind")
		}
	}
}

func TestInit_AnExistingLockBelongsToSomeoneElse(t *testing.T) {
	p := newPaths(t)
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(p.DataDir, ".init.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"})
	if !errors.Is(err, daemon.ErrInitInProgress) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Errorf("init removed a lock it did not create: %v", err)
	}
	if _, err := os.Lstat(p.ConfigDir); err == nil {
		t.Error("init created the config directory without holding the lock")
	}
}

func TestInit_RefusalAndFailureOnlyRemoveWhatInitCreated(t *testing.T) {
	// Pre-existing server key: init refuses and leaves it, and the directories around it.
	p := newPaths(t)
	if err := os.MkdirAll(p.PKIDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	serverKey := filepath.Join(p.PKIDir(), "server.key")
	if err := os.WriteFile(serverKey, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"})
	if !errors.Is(err, daemon.ErrAlreadyInitialized) {
		t.Fatalf("err = %v", err)
	}
	if got, err := os.ReadFile(serverKey); err != nil || string(got) != "mine" { //nolint:gosec // test path
		t.Fatalf("the server key was touched: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(p.DataDir, ".init.lock")); err == nil {
		t.Error("the lock was left behind")
	}

	// A failure midway keeps a data directory that existed before.
	p = newPaths(t)
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(p.DataDir, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.ConfigDir, 0o500); err != nil { //nolint:gosec // a read-only directory is the failure under test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.ConfigDir, 0o700) }) //nolint:gosec // restoring the test directory
	if _, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"}); err == nil {
		t.Skip("running as a user that ignores directory modes")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("init removed a file it did not create: %v", err)
	}
	if entries, _ := os.ReadDir(p.DataDir); len(entries) != 1 {
		t.Errorf("data directory holds %d entries after the failure, want only keep.txt", len(entries))
	}
}

func TestInit_RefusesDirectoriesOpenToOthers(t *testing.T) {
	for _, which := range []string{"data", "config"} {
		p := newPaths(t)
		open := p.DataDir
		if which == "config" {
			open = p.ConfigDir
		}
		if err := os.MkdirAll(open, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o755); err != nil { //nolint:gosec // the unsafe mode is the point of the test
			t.Fatal(err)
		}
		_, err := daemon.Init(t.Context(), daemon.InitOptions{Paths: p, Advertise: "127.0.0.1:7443"})
		if !errors.Is(err, daemon.ErrUnsafeDirectory) || !strings.Contains(err.Error(), "chmod 700") {
			t.Errorf("%s dir 0755: err = %v", which, err)
		}
		if entries, _ := os.ReadDir(open); len(entries) != 0 {
			t.Errorf("%s dir 0755: init wrote %d entries into it", which, len(entries))
		}
	}
}

func TestInit_TheDatabaseIsPrivate(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	if got := mode(t, p.DatabaseFile()); got != 0o600 {
		t.Errorf("database mode %04o, want 0600", got)
	}
	start(t, p) // a running Controller has the -wal and -shm files open
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(p.DatabaseFile() + side); err != nil {
			continue
		}
		if got := mode(t, p.DatabaseFile()+side); got != 0o600 {
			t.Errorf("%s mode %04o, want 0600", side, got)
		}
	}
}

func TestStart_RenewsTheServerCertificateWhileRunning(t *testing.T) {
	p := newPaths(t)
	_, addr := initController(t, p)
	clk := clock.NewFake(time.Now())
	ctx, cancel := context.WithCancel(t.Context())
	c, err := daemon.Start(ctx, daemon.Options{Paths: p, Clock: clk, ServerCertCheckEvery: time.Hour})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := c.Wait(); err != nil {
			t.Errorf("Wait: %v", err)
		}
	})

	presented := func() string {
		conn, err := (&tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}). //nolint:gosec // only the presented serial is read
																DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		return conn.(*tls.Conn).ConnectionState().PeerCertificates[0].SerialNumber.Text(16) //nolint:forcetypeassert // tls.Dialer returns *tls.Conn
	}
	before := presented()
	if again := presented(); again != before {
		t.Fatal("the certificate changed without approaching its expiry")
	}

	clk.BlockUntil(2)                 // the renewal and the sweeper are waiting on the clock
	clk.Advance(340 * 24 * time.Hour) // under 30 days left
	deadline := time.Now().Add(5 * time.Second)
	for presented() == before {
		if time.Now().After(deadline) {
			t.Fatal("the running Controller kept presenting the certificate that is about to expire")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
