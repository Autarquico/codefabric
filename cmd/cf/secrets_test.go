package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// sentinel is a value no output, request or error may carry.
const sentinel = "sentinel-7f3a9c1e-never-leaves-this-machine"

// leaks says whether text carries the sentinel as text, hex or base64.
func leaks(text string) bool {
	for _, form := range []string{
		sentinel, hex.EncodeToString([]byte(sentinel)),
		base64.StdEncoding.EncodeToString([]byte(sentinel)), base64.RawURLEncoding.EncodeToString([]byte(sentinel)),
	} {
		if strings.Contains(text, form) {
			return true
		}
	}
	return false
}

// recorded is a request the Worker sent through the capture proxy, and the
// status the Controller answered it with.
type recorded struct {
	path   string
	body   []byte
	status int
}

// statusWriter keeps the status of the answer it forwards.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// captureProxy stands between a Worker and its Controller, and records every
// request the Worker sends. It terminates TLS with the Controller's own server
// certificate and dials on with the Worker's credentials, so both ends see
// what they would see without it. A request is recorded once the Controller
// has answered it, so what the proxy shows the Controller has already done.
type captureProxy struct {
	addr string
	mu   sync.Mutex
	seen []recorded
}

func newCaptureProxy(t *testing.T, host, worker config.Paths) *captureProxy {
	t.Helper()
	read := func(path string) []byte {
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the test
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	server, err := tls.LoadX509KeyPair(filepath.Join(host.PKIDir(), "server.pem"), filepath.Join(host.PKIDir(), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	creds := worker.WorkerCredentialsDir()
	client, err := tls.X509KeyPair(read(filepath.Join(creds, "cert.pem")), read(filepath.Join(creds, "key.pem")))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(read(filepath.Join(creds, "ca.pem")))
	upstream := strings.TrimSpace(string(read(filepath.Join(creds, "controller"))))
	upstreamHost, _, _ := net.SplitHostPort(upstream)

	p := &captureProxy{}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "https", upstream, upstream
		},
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: upstreamHost, Certificates: []tls.Certificate{client},
		}},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0), // the requests the end of the test cuts
	}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{server}, ClientAuth: tls.RequireAnyClientCert},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			forward.ServeHTTP(sw, r)
			p.mu.Lock()
			p.seen = append(p.seen, recorded{path: r.URL.Path, body: body, status: sw.status})
			p.mu.Unlock()
		}),
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

func (p *captureProxy) requests() []recorded {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// heartbeatWith waits for a heartbeat that reports exactly refs and that the
// Controller took: from then on, the Controller holds them.
func (p *captureProxy) heartbeatWith(t *testing.T, refs ...string) v1.HeartbeatRequest {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, r := range p.requests() {
			var hb v1.HeartbeatRequest
			if r.path != "/v1/worker/heartbeat" || r.status != http.StatusOK || json.Unmarshal(r.body, &hb) != nil || hb.Secrets == nil {
				continue
			}
			var got []string
			for _, m := range hb.Secrets.Refs {
				got = append(got, m.Ref)
			}
			if slices.Equal(got, refs) {
				return hb
			}
		}
	}
	t.Fatalf("no heartbeat reported the secrets %v", refs)
	return v1.HeartbeatRequest{}
}

// setSecret runs `cf worker secrets set` with the value on stdin.
func setSecret(t *testing.T, value string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return cf(t, value, append([]string{"worker", "secrets", "set"}, args...)...)
}

// TestScenario_SecretsLocal_SoloMetadata: a secret set on a Worker reaches the
// Controller as metadata, at the next heartbeat; its value never leaves the
// Worker. What the Controller receives is captured on the wire, between the two.
func TestScenario_SecretsLocal_SoloMetadata(t *testing.T) {
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "box")
	proxy := newCaptureProxy(t, host, box)

	// Given a secret set on the Worker, which talks to no one.
	if code, out, errOut := setSecret(t, sentinel+"\n", "github/token"); code != 0 || !strings.Contains(out, "next heartbeat") || leaks(out+errOut) {
		t.Fatalf("set: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("cf worker secrets set talked to the Controller")
	}

	// When the Worker heartbeats, through the proxy.
	use(t, box)
	ctx, interrupt := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if code, errOut := cfContext(ctx, "worker", "run", "--controller", proxy.addr, "--heartbeat-interval", "50ms"); code != 0 {
			t.Errorf("worker run: exit %d: %s", code, errOut)
		}
	}()
	defer func() { interrupt(); <-done }()

	// Then the Controller gets the ref and its metadata.
	hb := proxy.heartbeatWith(t, "github/token")
	if m := hb.Secrets.Refs[0]; m.Backend != v1.SecretBackendFile || m.UpdatedAt.IsZero() {
		t.Errorf("metadata = %+v", m)
	}
	// A secret set while the Worker runs reaches the next heartbeat.
	if code, _, errOut := setSecret(t, sentinel+"-2", "npm/token"); code != 0 {
		t.Fatalf("second set: exit %d: %s", code, errOut)
	}
	proxy.heartbeatWith(t, "github/token", "npm/token")
	w := showWorker(t, host, "box")
	if len(w.Secrets) != 2 {
		t.Errorf("the Controller shows %+v", w.Secrets)
	}

	// And no request carried the value, in any form.
	for _, r := range proxy.requests() {
		if leaks(string(r.body)) || leaks(r.path) {
			t.Errorf("a request to %s carried the value", r.path)
		}
	}
	use(t, host)
	if code, out, _ := cf(t, "", "worker", "show", "box", "--json"); code != 0 || leaks(out) {
		t.Errorf("worker show: exit %d, leaks %v", code, leaks(out))
	}
}

func TestCLI_WorkerSecretsSetListRm(t *testing.T) {
	box := machine(t)
	store := secretsfile.New(box.WorkerSecretsFile(), clock.Real{})

	// Exactly one final newline is not part of the value.
	if code, _, errOut := setSecret(t, sentinel+"\n\n", "github/token"); code != 0 {
		t.Fatalf("set: exit %d: %s", code, errOut)
	}
	if v, err := store.Get(t.Context(), "github/token"); err != nil || string(v) != sentinel+"\n" {
		t.Errorf("stored %q, %v", v, err)
	}
	project := "project_01J0000000000000000000000Z"
	meta := func() v1.SecretMeta {
		code, out, errOut := setSecret(t, sentinel, "npm/token", "--project", project, "--json")
		if code != 0 || leaks(out+errOut) {
			t.Fatalf("set --json: exit %d: %s %s", code, out, errOut)
		}
		var m v1.SecretMeta
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}()
	if meta.Ref != "npm/token" || meta.Backend != "file" || !slices.Equal(meta.Projects, []string{project}) || meta.UpdatedAt.IsZero() {
		t.Errorf("set --json = %+v", meta)
	}

	listed := ok[struct {
		Secrets []v1.SecretMeta `json:"secrets"`
	}](t, "worker", "secrets", "list")
	if len(listed.Secrets) != 2 || listed.Secrets[0].Ref != "github/token" || listed.Secrets[1].Ref != "npm/token" {
		t.Errorf("list --json = %+v", listed)
	}
	code, out, _ := cf(t, "", "worker", "secrets", "list")
	if code != 0 || leaks(out) || !strings.Contains(out, "REF") || !strings.Contains(out, "any Project") || !strings.Contains(out, project) {
		t.Errorf("list: exit %d:\n%s", code, out)
	}
	if raw, _ := json.Marshal(listed); leaks(string(raw)) {
		t.Error("list --json carries the value")
	}

	if code, out, _ := cf(t, "", "worker", "secrets", "rm", "npm/token"); code != 0 || !strings.Contains(out, "Removed secret npm/token") {
		t.Errorf("rm: exit %d: %s", code, out)
	}
	fails(t, 3, domain.CodeNotFound, "worker", "secrets", "rm", "npm/token")
	if code, out, _ := cf(t, "", "worker", "secrets", "rm", "github/token"); code != 0 {
		t.Errorf("rm: exit %d: %s", code, out)
	}
	if code, out, _ := cf(t, "", "worker", "secrets", "list"); code != 0 || !strings.Contains(out, "no secrets") {
		t.Errorf("empty list: exit %d: %s", code, out)
	}
	if got := ok[map[string][]v1.SecretMeta](t, "worker", "secrets", "list"); got["secrets"] == nil {
		t.Errorf("an empty list is an empty array: %v", got)
	}
}

// TestCLI_WorkerSecretsErrorsNeverRepeatTheirArguments: any argument may be a
// value typed in the wrong place, so no refusal repeats one, and none stores anything.
func TestCLI_WorkerSecretsErrorsNeverRepeatTheirArguments(t *testing.T) {
	box := machine(t)
	for name, tc := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"value as an argument":       {sentinel, []string{"set", "github/token", sentinel}, "never as an argument"},
		"value as --value=":          {"", []string{"set", "github/token", "--value=" + sentinel}, "never a flag"},
		"value after --value":        {"", []string{"set", "github/token", "--value", sentinel}, "never a flag"},
		"value as a lone --value":    {sentinel, []string{"set", "github/token", "--value"}, "never a flag"},
		"value as the ref":           {sentinel, []string{"set", sentinel + "!"}, "ref is not valid"},
		"value as a Project":         {sentinel, []string{"set", "github/token", "--project", sentinel}, "Project ID"},
		"value as a shorthand flag":  {sentinel, []string{"set", "github/token", "-x" + sentinel}, "unknown or lacks its value"},
		"value as an unknown flag":   {sentinel, []string{"set", "github/token", "--secret=" + sentinel}, "unknown or lacks its value"},
		"value as a subcommand":      {"", []string{sentinel}, "takes a subcommand"},
		"value as the ref to remove": {"", []string{"rm", sentinel}, "no secret by that ref"},
		"value as a ref to remove":   {"", []string{"rm", sentinel + "!"}, "ref is not valid"},
		"value too short":            {"short", []string{"set", "github/token"}, "fewer than 8 bytes"},
		"no value":                   {"", []string{"set", "github/token"}, "no value given"},
		"value too long":             {strings.Repeat("x", domain.MaxSecretValueLen+1), []string{"set", "github/token"}, "more than 65536 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, asJSON := range []bool{false, true} {
				args := append([]string{"worker", "secrets"}, tc.args...)
				if asJSON {
					args = append(args, "--json")
				}
				code, out, errOut := cf(t, tc.stdin, args...)
				if code == 0 || leaks(out+errOut) || !strings.Contains(out+errOut, tc.want) {
					t.Errorf("json=%v: exit %d\nstdout: %s\nstderr: %s", asJSON, code, out, errOut)
				}
			}
		})
	}
	if _, err := os.Stat(box.WorkerSecretsFile()); !os.IsNotExist(err) {
		t.Errorf("a refused command wrote the catalog: %v", err)
	}
}

func TestCLI_ProjectSecretsAllowDenyList(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	p := ok[v1.Project](t, "project", "create", "backend")

	allowed := ok[v1.ProjectSecrets](t, "project", "secrets", "allow", "backend", "npm/token", "github/token", "--request-id", "r1")
	if allowed.ProjectID != p.ID || !slices.Equal(allowed.Allowed, []string{"github/token", "npm/token"}) {
		t.Fatalf("allow = %+v", allowed)
	}
	// Repeating a request with its request_id changes nothing twice, even after
	// another change; the same request_id with other refs is refused.
	ok[v1.ProjectSecrets](t, "project", "secrets", "deny", "backend", "npm/token", "--request-id", "r2")
	if again := ok[v1.ProjectSecrets](t, "project", "secrets", "allow", "backend", "npm/token", "github/token", "--request-id", "r1"); !slices.Equal(again.Allowed, allowed.Allowed) {
		t.Errorf("the repeated allow answered %+v", again)
	}
	if listed := ok[v1.ProjectSecrets](t, "project", "secrets", "list", "backend"); !slices.Equal(listed.Allowed, []string{"github/token"}) {
		t.Errorf("the repeated allow allowed again: %+v", listed)
	}
	fails(t, 4, domain.CodeIdempotencyMismatch, "project", "secrets", "allow", "backend", "other/token", "--request-id", "r1")
	if again := ok[v1.ProjectSecrets](t, "project", "secrets", "deny", "backend", "npm/token", "--request-id", "r2"); !slices.Equal(again.Allowed, []string{"github/token"}) {
		t.Errorf("the repeated deny answered %+v", again)
	}
	// Without a request_id, a ref already allowed or already out changes nothing.
	if again := ok[v1.ProjectSecrets](t, "project", "secrets", "deny", "backend", "npm/token"); !slices.Equal(again.Allowed, []string{"github/token"}) {
		t.Errorf("denying a ref already out: %+v", again)
	}

	code, out, _ := cf(t, "", "project", "secrets", "list", "backend")
	if code != 0 || !strings.Contains(out, "REF") || !strings.Contains(out, "github/token") || !strings.Contains(out, "-") {
		t.Errorf("list: exit %d:\n%s", code, out)
	}
	if code, out, _ := cf(t, "", "project", "secrets", "allow", "backend", "npm/token"); code != 0 || !strings.Contains(out, "allows github/token, npm/token") {
		t.Errorf("allow: exit %d: %s", code, out)
	}
	ok[v1.ProjectSecrets](t, "project", "secrets", "deny", "backend", "npm/token", "github/token")
	if code, out, _ := cf(t, "", "project", "secrets", "list", "backend"); code != 0 || !strings.Contains(out, "allows no secrets") {
		t.Errorf("empty list: exit %d: %s", code, out)
	}
	fails(t, 2, domain.CodeInvalidRequest, "project", "secrets", "allow", "backend", "a*")
	fails(t, 3, domain.CodeNotFound, "project", "secrets", "list", "nowhere")
	fails(t, 2, domain.CodeInvalidRequest, "project", "secrets", "allow", "backend")
}

func TestCLI_TaskSecretEnv(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")

	created := ok[v1.CreateTaskResponse](t, "task", "new", "x", "--no-start", "--secret-env", "GH_TOKEN=github/token")
	if env := created.Task.Requirements.SecretEnv; len(env) != 1 || env[0] != (v1.SecretEnv{Name: "GH_TOKEN", Ref: "github/token"}) {
		t.Errorf("requirements = %+v", created.Task.Requirements)
	}
	fails(t, 2, domain.CodeInvalidRequest, "task", "new", "y", "--secret-env", "GH_TOKEN")
	fails(t, 2, domain.CodeInvalidRequest, "task", "new", "y", "--secret-env", "PATH=github/token")
}
