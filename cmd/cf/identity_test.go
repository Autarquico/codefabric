package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
)

// cf runs the real command tree, composed exactly as main does.
func cf(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cliRun(&out, &errOut, stdin, args)
	return code, out.String(), errOut.String()
}

func cliRun(stdout, stderr io.Writer, stdin string, args []string) int {
	return cli.Run(args, strings.NewReader(stdin), stdout, stderr, daemon.Command())
}

// machine points the CLI at a fresh config/data directory pair.
func machine(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	p := config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")}
	use(t, p)
	return p
}

func use(t *testing.T, p config.Paths) {
	t.Helper()
	t.Setenv("CF_CONFIG_DIR", p.ConfigDir)
	t.Setenv("CF_DATA_DIR", p.DataDir)
}

var (
	inviteRe   = regexp.MustCompile(`cf://\S+`)
	identityRe = regexp.MustCompile(`as (client_\w+)`)
)

// initController inits a Controller on a free localhost port, without serving it.
func initController(t *testing.T) (host config.Paths, addr, firstInvite string) {
	t.Helper()
	host = machine(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()
	_ = ln.Close()

	code, out, errOut := cf(t, "", "controller", "init", "--advertise", addr, "--listen", addr, "--name", "host")
	if code != 0 {
		t.Fatalf("init: exit %d: %s", code, errOut)
	}
	return host, addr, inviteRe.FindString(out)
}

// serve runs the Controller of paths until the returned stop is called (or the test ends).
func serve(t *testing.T, paths config.Paths, opts daemon.Options) (stop func()) {
	t.Helper()
	opts.Paths = paths
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(t.Context())
	c, err := daemon.Start(ctx, opts)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := c.Wait(); err != nil {
				t.Errorf("Wait: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// bringUpController inits a Controller on a free localhost port and serves it.
func bringUpController(t *testing.T) (host config.Paths, firstInvite string) {
	t.Helper()
	host, _, firstInvite = initController(t)
	serve(t, host, daemon.Options{})
	return host, firstInvite
}

func TestScenario_ControllerIdentityTLS_Revocacion(t *testing.T) {
	host, invite := bringUpController(t)
	if invite == "" {
		t.Fatal("init printed no invitation")
	}

	laptop := machine(t)
	code, out, errOut := cf(t, invite+"\n", "connect", "-", "--name", "laptop")
	if code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}
	id := identityRe.FindStringSubmatch(out)[1]
	if code, out, errOut = cf(t, "", "client", "list"); code != 0 || !strings.Contains(out, "laptop") {
		t.Fatalf("list before revocation: exit %d\n%s%s", code, out, errOut)
	}

	use(t, host)
	if code, out, errOut = cf(t, "", "client", "remove", id); code != 0 {
		t.Fatalf("remove: exit %d: %s%s", code, out, errOut)
	}

	use(t, laptop)
	for _, args := range [][]string{{"client", "list"}, {"client", "renew"}, {"controller", "token", "create", "--client"}} {
		code, _, errOut = cf(t, "", args...)
		if code != 6 || !strings.Contains(errOut, "CF_UNAUTHENTICATED") || !strings.Contains(errOut, "cf connect --force") {
			t.Errorf("cf %v after revocation: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestCLI_ConnectWithoutCredentialsSuggestsConnect(t *testing.T) {
	machine(t)
	code, _, errOut := cf(t, "", "client", "list")
	if code != 6 || !strings.Contains(errOut, "cf connect") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestCLI_ConnectReadsTheInvitationFromArgumentOrPrompt(t *testing.T) {
	_, invite := bringUpController(t)
	code, out, errOut := cf(t, "", "controller", "token", "create", "--client")
	if code != 0 {
		t.Fatalf("token create: exit %d: %s", code, errOut)
	}
	second := strings.TrimSpace(out)

	machine(t)
	if code, _, errOut = cf(t, "", "connect", invite); code != 0 {
		t.Fatalf("argument: exit %d: %s", code, errOut)
	}

	machine(t)
	code, _, errOut = cf(t, second+"\n", "connect")
	if code != 0 || !strings.Contains(errOut, "Invitation:") {
		t.Fatalf("prompt: exit %d: %q", code, errOut)
	}

	machine(t)
	if code, _, errOut = cf(t, "", "connect"); code != 2 || !strings.Contains(errOut, "CF_INVALID_REQUEST") || !strings.Contains(errOut, "no invitation") {
		t.Fatalf("empty stdin: exit %d: %q", code, errOut)
	}
	if code, _, _ = cf(t, "", "connect", "https://example.com"); code != 2 {
		t.Fatalf("malformed invitation: exit %d", code)
	}
}

func TestCLI_WorkerInvitationsAreIssuedButNotRedeemable(t *testing.T) {
	host, _ := bringUpController(t)
	code, out, errOut := cf(t, "", "controller", "token", "create", "--worker", "--ttl", "10m")
	if code != 0 || !inviteRe.MatchString(out) {
		t.Fatalf("create: exit %d: %q %q", code, out, errOut)
	}
	invite := strings.TrimSpace(out)
	if strings.Contains(errOut, invite) {
		t.Error("the invitation was printed twice")
	}

	machine(t)
	code, _, errOut = cf(t, "", "connect", invite)
	if code != 8 || !strings.Contains(errOut, "CF_NOT_IMPLEMENTED") {
		t.Fatalf("redeem: exit %d, stderr %q", code, errOut)
	}
	use(t, host)
	if code, _, _ = cf(t, "", "controller", "token", "create"); code != 2 {
		t.Errorf("token create without --client/--worker: exit %d", code)
	}
	if code, _, _ = cf(t, "", "controller", "token", "create", "--client", "--worker"); code != 2 {
		t.Errorf("token create with both kinds: exit %d", code)
	}
	if code, _, errOut = cf(t, "", "controller", "token", "create", "--client", "--ttl", "200h"); code != 2 || !strings.Contains(errOut, "ttl") {
		t.Errorf("ttl above the cap: exit %d, stderr %q", code, errOut)
	}
}

func TestCLI_RenewReplacesTheCertificate(t *testing.T) {
	_, invite := bringUpController(t)
	laptop := machine(t)
	if code, _, errOut := cf(t, "", "connect", invite); code != 0 {
		t.Fatalf("connect: %s", errOut)
	}
	before, _ := os.ReadFile(filepath.Join(laptop.CredentialsDir(), "cert.pem"))
	if code, out, errOut := cf(t, "", "client", "renew"); code != 0 || !strings.Contains(out, "renewed") {
		t.Fatalf("renew: exit %d: %q %q", code, out, errOut)
	}
	after, _ := os.ReadFile(filepath.Join(laptop.CredentialsDir(), "cert.pem"))
	if bytes.Equal(before, after) {
		t.Error("the certificate did not change")
	}
	if code, _, errOut := cf(t, "", "client", "list"); code != 0 {
		t.Fatalf("the renewed certificate does not work: %s", errOut)
	}
}

func TestCLI_ConfigPathsAndShowRedactSecrets(t *testing.T) {
	p := machine(t)
	code, out, _ := cf(t, "", "config", "paths")
	if code != 0 || !strings.Contains(out, p.ConfigDir) || !strings.Contains(out, p.DataDir) || !strings.Contains(out, p.CredentialsDir()) {
		t.Fatalf("paths: exit %d\n%s", code, out)
	}
	if code, out, _ = cf(t, "", "config", "show"); code != 0 || !strings.Contains(out, "not connected") {
		t.Fatalf("show before init: exit %d\n%s", code, out)
	}

	host, invite := bringUpController(t)
	code, out, errOut := cf(t, "", "config", "show")
	if code != 0 {
		t.Fatalf("show: exit %d: %s", code, errOut)
	}
	token := regexp.MustCompile(`token=([^&#\s]+)`).FindStringSubmatch(invite)[1]
	keyPEM, _ := os.ReadFile(filepath.Join(host.CredentialsDir(), "key.pem"))
	body := strings.Join(strings.Split(strings.TrimSpace(string(keyPEM)), "\n")[1:2], "")
	for _, secret := range []string{"PRIVATE KEY", body, token, "cf://"} {
		if strings.Contains(out, secret) {
			t.Errorf("config show leaked %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "[redacted]") || !strings.Contains(out, "client_") {
		t.Errorf("config show lacks the identity/redaction marker:\n%s", out)
	}
}

func TestCLI_FingerprintMismatchExitsWithAuthenticationError(t *testing.T) {
	_, invite := bringUpController(t)
	forged := regexp.MustCompile(`#sha256:[0-9a-f]+`).ReplaceAllString(invite, "#sha256:"+strings.Repeat("ab", 32))

	laptop := machine(t)
	code, _, errOut := cf(t, "", "connect", forged)
	if code != 6 || !strings.Contains(errOut, "nothing was sent") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(laptop.CredentialsDir()); err == nil {
		t.Error("credentials were written")
	}
}

func TestCLI_ControllerRunServesUntilCancelled(t *testing.T) {
	machine(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if code, _, errOut := cf(t, "", "controller", "init", "--advertise", addr, "--listen", addr); code != 0 {
		t.Fatal(errOut)
	}

	root := daemon.Command()
	var errOut syncBuffer
	root.SetErr(&errOut)
	root.SetArgs([]string{"run"})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(errOut.String(), "listening on "+addr) {
		if time.Now().After(deadline) {
			t.Fatalf("the Controller never reported listening: %q", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, out, e := cf(t, "", "client", "list"); code != 0 || !strings.Contains(out, "owner") && !strings.Contains(out, "client_") {
		t.Fatalf("list against `controller run`: exit %d\n%s%s", code, out, e)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned %v", err)
	}
}

func TestCLI_ConnectRefusesToReplaceCredentialsWithoutForce(t *testing.T) {
	host, first := bringUpController(t)
	laptop := machine(t)
	code, out, errOut := cf(t, "", "connect", first, "--name", "laptop")
	if code != 0 {
		t.Fatalf("first connect: exit %d: %s", code, errOut)
	}
	firstID := identityRe.FindStringSubmatch(out)[1]

	use(t, host)
	code, out, _ = cf(t, "", "controller", "token", "create", "--client")
	if code != 0 {
		t.Fatalf("token create: exit %d", code)
	}
	second := strings.TrimSpace(out)

	use(t, laptop)
	before, _ := os.ReadFile(filepath.Join(laptop.CredentialsDir(), "cert.pem"))
	code, _, errOut = cf(t, "", "connect", second)
	if code == 0 || !strings.Contains(errOut, "--force") {
		t.Fatalf("connect over existing credentials: exit %d, stderr %q", code, errOut)
	}
	if after, _ := os.ReadFile(filepath.Join(laptop.CredentialsDir(), "cert.pem")); !bytes.Equal(before, after) {
		t.Fatal("the credentials changed without --force")
	}

	// The refusal came before the join: the invitation is still good.
	code, out, errOut = cf(t, "", "connect", second, "--force", "--name", "laptop-2")
	if code != 0 {
		t.Fatalf("connect --force: exit %d: %s", code, errOut)
	}
	if id := identityRe.FindStringSubmatch(out)[1]; id == firstID {
		t.Error("--force did not enrol a new identity")
	}
}
