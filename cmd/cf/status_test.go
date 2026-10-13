package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// jsonValue decodes stdout, which must hold exactly one JSON object.
func jsonValue(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not JSON: %q: %v", stdout, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("stdout holds more than one JSON value: %q", stdout)
	}
	return v
}

// errorOf is the error object of a failed command's stdout.
func errorOf(t *testing.T, stdout string) map[string]any {
	t.Helper()
	env, ok := jsonValue(t, stdout)["error"].(map[string]any)
	if !ok {
		t.Fatalf("stdout is not an error envelope: %q", stdout)
	}
	return env
}

func statusOf(t *testing.T) v1.Status {
	t.Helper()
	code, out, errOut := cf(t, "", "status", "--json")
	if code != 0 {
		t.Fatalf("status: exit %d: %s%s", code, out, errOut)
	}
	var st v1.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestScenario_ApiSdkCliStatus_Status(t *testing.T) {
	host, addr, invite := initController(t)
	clk := clock.NewFake(time.Now().UTC())
	started := clk.Now()
	serve(t, host, daemon.Options{Clock: clk})

	machine(t)
	if code, _, errOut := cf(t, invite+"\n", "connect", "-", "--name", "laptop"); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}
	clk.Advance(90 * time.Second)

	code, out, errOut := cf(t, "", "status", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("status --json: exit %d: %s", code, errOut)
	}
	var st v1.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	// The real values, before they are normalised for the golden file.
	build := buildinfo.Get()
	if st.Version != build.Version || st.Commit != build.Commit {
		t.Errorf("build %q/%q, want %q/%q", st.Version, st.Commit, build.Version, build.Commit)
	}
	if st.UptimeSeconds != 90 || !st.StartedAt.Equal(started) {
		t.Errorf("uptime %d from %s, want 90 from %s", st.UptimeSeconds, st.StartedAt, started)
	}
	if !st.Database.OK || st.Database.Error != "" {
		t.Errorf("database %+v", st.Database)
	}
	if !ulidRe.MatchString(st.ControllerID) {
		t.Errorf("controller id %q", st.ControllerID)
	}
	assertGolden(t, "status.golden.json", normalised(t, out))

	code, out, errOut = cf(t, "", "status")
	if code != 0 || errOut != "" {
		t.Fatalf("status: exit %d: %s", code, errOut)
	}
	for _, want := range []string{"Controller  " + addr, "ID          " + st.ControllerID, "Version     " + build.Version, "Uptime      1m30s", "Database    ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output lacks %q:\n%s", want, out)
		}
	}
}

// normalised replaces the values that change from run to run, keeping every key.
func normalised(t *testing.T, stdout string) string {
	t.Helper()
	v := jsonValue(t, stdout)
	for _, key := range []string{"controller_id", "version", "commit", "started_at", "uptime_seconds"} {
		if _, ok := v[key]; !ok {
			t.Fatalf("status lacks %q: %s", key, stdout)
		}
		v[key] = "<" + key + ">"
	}
	var pretty strings.Builder
	enc := json.NewEncoder(&pretty)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return pretty.String()
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test path
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

func TestScenario_ApiSdkCliStatus_ControllerCaido(t *testing.T) {
	host, _, invite := initController(t)
	stop := serve(t, host, daemon.Options{})
	machine(t)
	if code, _, errOut := cf(t, invite+"\n", "connect", "-"); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}
	stop()

	code, out, errOut := cf(t, "", "status", "--json")
	env := errorOf(t, out)
	if code != 6 || env["code"] != string(domain.CodeControllerUnreachable) || errOut != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	// A local failure has a local cause, and --debug shows it.
	code, _, debugErr := cf(t, "", "status", "--debug")
	if code != 6 || !strings.Contains(debugErr, "cause: ") {
		t.Fatalf("--debug: exit %d, stderr %q", code, debugErr)
	}
}

func TestScenario_ApiSdkCliStatus_Persistencia(t *testing.T) {
	host, _, invite := initController(t)
	stop := serve(t, host, daemon.Options{})

	laptop := machine(t)
	if code, _, errOut := cf(t, invite+"\n", "connect", "-", "--name", "laptop"); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}
	use(t, host)
	before := statusOf(t)
	code, clientsBefore, errOut := cf(t, "", "client", "list", "--json")
	if code != 0 {
		t.Fatalf("client list before the restart: exit %d: %s", code, errOut)
	}
	stop()

	serve(t, host, daemon.Options{})
	after := statusOf(t)
	code, clientsAfter, errOut := cf(t, "", "client", "list", "--json")
	if code != 0 {
		t.Fatalf("client list after the restart: exit %d: %s", code, errOut)
	}

	if before.ControllerID == "" || after.ControllerID != before.ControllerID {
		t.Errorf("controller id %q before, %q after the restart", before.ControllerID, after.ControllerID)
	}
	if !after.StartedAt.After(before.StartedAt) {
		t.Errorf("started %s then %s", before.StartedAt, after.StartedAt)
	}
	if clientsAfter != clientsBefore || !strings.Contains(clientsAfter, `"name":"laptop"`) {
		t.Errorf("clients changed over the restart:\n%s\n%s", clientsBefore, clientsAfter)
	}
	// The credential issued before the restart is still good.
	use(t, laptop)
	if got := statusOf(t); got.ControllerID != before.ControllerID {
		t.Errorf("laptop sees controller %q", got.ControllerID)
	}
}

func TestScenario_ApiSdkCliStatus_SinCredencial(t *testing.T) {
	machine(t)
	code, out, errOut := cf(t, "", "status")
	if code != 6 || out != "" || !strings.Contains(errOut, "CF_UNAUTHENTICATED") || !strings.Contains(errOut, "cf connect <invitation>") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// serveWithPanickingStatus runs the Controller of paths like the daemon does,
// except that its database health check panics. It returns the server log.
//
// It replicates daemon.Start (wiring of the CA, server certificate, store,
// authority and api.Server): keep the two in sync when Start changes.
func serveWithPanickingStatus(t *testing.T, p config.Paths) *syncBuffer {
	t.Helper()
	cfg, err := config.LoadController(p)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(cfg.Advertise)
	ca, err := pki.LoadCA(p.PKIDir())
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := ca.LoadServerCert(p.PKIDir(), host, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), p.DatabaseFile(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	clk := clock.Real{}
	srv, err := api.New(api.Config{
		Authority: pki.NewAuthority(pki.AuthorityConfig{
			DB: db, CA: ca, Clock: clk, IDs: domain.NewIDGenerator(clk.Now, rand.Reader),
			CertTTL: domain.Defaults().IdentityCertTTL, Host: host, Port: port,
		}),
		CA: ca, ServerCert: serverCert, Store: panickingHealth{db}, Clock: clk, StartedAt: clk.Now(),
		Work:   app.New(db, clk, domain.NewIDGenerator(clk.Now, rand.Reader), app.Config{}),
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = db.Close()
	})
	return logs
}

type panickingHealth struct{ *store.DB }

func (panickingHealth) Health(context.Context) error { panic("boom-secret") }

var requestRe = regexp.MustCompile(`\(request (\w+)\)`)

func TestScenario_ApiSdkCliStatus_ErrorInterno(t *testing.T) {
	host, _, _ := initController(t)
	logs := serveWithPanickingStatus(t, host)
	use(t, host)

	code, out, errOut := cf(t, "", "status")
	m := requestRe.FindStringSubmatch(errOut)
	if code != 1 || out != "" || m == nil || strings.TrimSpace(errOut) != "error: CF_INTERNAL: internal error "+m[0] {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	id := m[1]
	assertNoServerInternals(t, errOut)

	code, _, debugErr := cf(t, "", "status", "--debug")
	if code != 1 || !strings.Contains(debugErr, "request_id: ") {
		t.Fatalf("--debug: exit %d, stderr %q", code, debugErr)
	}
	assertNoServerInternals(t, debugErr)

	code, out, _ = cf(t, "", "status", "--json")
	env := errorOf(t, out)
	details, _ := env["details"].(map[string]any)
	jsonID, _ := details["request_id"].(string)
	if code != 1 || env["code"] != string(domain.CodeInternal) || jsonID == "" {
		t.Fatalf("--json: exit %d, stdout %q", code, out)
	}
	assertNoServerInternals(t, out)

	log := logs.String()
	if !strings.Contains(log, "request_id="+jsonID) {
		t.Errorf("the --json request_id %q is not in the server log:\n%s", jsonID, log)
	}
	if !strings.Contains(log, "request_id="+id) || !strings.Contains(log, "boom-secret") || !strings.Contains(log, "goroutine") {
		t.Errorf("the stack and the request %s belong in the server log:\n%s", id, log)
	}
}

func assertNoServerInternals(t *testing.T, shown string) {
	t.Helper()
	for _, leak := range []string{"boom-secret", "goroutine", ".go:", "panic"} {
		if strings.Contains(shown, leak) {
			t.Errorf("output leaks %q: %s", leak, shown)
		}
	}
}

func TestScenario_ApiSdkCliStatus_ErrorInternoDeUnaOperacionInesperada(t *testing.T) {
	host, _, invite := initController(t)
	serve(t, host, daemon.Options{})
	machine(t)
	if code, _, errOut := cf(t, invite+"\n", "connect", "-"); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errOut)
	}

	// Break the database under the running Controller.
	db, err := sql.Open("sqlite", host.DatabaseFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE identities RENAME TO identities_gone`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	for _, args := range [][]string{{"status"}, {"client", "list"}} {
		code, out, errOut := cf(t, "", args...)
		if code != 1 || out != "" || !requestRe.MatchString(errOut) {
			t.Fatalf("cf %v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
		if strings.Contains(errOut, "no such table") || strings.Contains(errOut, "identities") {
			t.Errorf("cf %v leaks the cause: %s", args, errOut)
		}
		_, _, debugErr := cf(t, "", append(args, "--debug")...)
		if strings.Contains(debugErr, "no such table") || strings.Contains(debugErr, "goroutine") {
			t.Errorf("cf %v --debug leaks server internals: %s", args, debugErr)
		}
	}
}

func TestApiSdkCliStatus_ControllerFlagKeepsTheTrustAndFailsLoudly(t *testing.T) {
	host, addr, _ := initController(t)
	serve(t, host, daemon.Options{})
	use(t, host)
	_, port, _ := net.SplitHostPort(addr)

	if code, out, errOut := cf(t, "", "status", "--controller", addr, "--json"); code != 0 {
		t.Fatalf("same address: exit %d: %s%s", code, out, errOut)
	}
	code, out, _ := cf(t, "", "status", "--controller", "localhost:"+port, "--json")
	env := errorOf(t, out)
	if code != 6 || env["code"] != string(domain.CodeControllerUnreachable) || !strings.Contains(fmt.Sprint(env["message"]), "--advertise") {
		t.Fatalf("uncovered host: exit %d, stdout %q", code, out)
	}
	for _, bad := range []string{"nonsense", ":" + port, "localhost:0", "localhost:99999", "user@" + addr, addr + "/path", addr + "?q", addr + "#f"} {
		if code, _, errOut := cf(t, "", "status", "--controller", bad); code != 2 || !strings.Contains(errOut, "CF_INVALID_REQUEST") {
			t.Fatalf("bad address %q: exit %d, stderr %q", bad, code, errOut)
		}
	}
	// The stored address is untouched.
	if stored, _ := os.ReadFile(filepath.Join(host.CredentialsDir(), "controller")); strings.TrimSpace(string(stored)) != addr { //nolint:gosec // test path
		t.Errorf("stored address %q, want %q", stored, addr)
	}
}

func TestApiSdkCliStatus_EveryCommandWithJSONWritesOneValue(t *testing.T) {
	host, _, invite := initController(t)
	serve(t, host, daemon.Options{})

	run := func(stdin string, args ...string) map[string]any {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := cliRun(&out, &errOut, stdin, append(args, "--json")); code != 0 || errOut.Len() != 0 {
			t.Fatalf("cf %v: exit %d, stderr %q, stdout %q", args, code, errOut.String(), out.String())
		}
		return jsonValue(t, out.String())
	}

	machine(t)
	connected := run(invite+"\n", "connect", "-", "--name", "laptop")
	run("", "status")
	run("", "client", "renew")
	run("", "client", "list")
	run("", "config", "paths")
	run("", "config", "show")
	run("", "version")

	use(t, host)
	run("", "controller", "token", "create", "--client")
	if got := run("", "client", "remove", connected["identity_id"].(string)); got["revoked"] != connected["identity_id"] {
		t.Errorf("remove: %v", got)
	}

	machine(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	run("", "controller", "init", "--advertise", addr, "--listen", addr)
}
