package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

// recordedRequest is what reached the fake Controller.
type recordedRequest struct{ Method, URI, Body string }

// fakeController answers every request with an empty JSON object and records it.
// It presents the certificate of the real Controller's CA, so the client trusts it.
type fakeController struct {
	addr string
	mu   sync.Mutex
	seen []recordedRequest
}

func newFakeController(t *testing.T, host config.Paths) *fakeController {
	t.Helper()
	ca, err := pki.LoadCA(host.PKIDir())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.LoadServerCert(host.PKIDir(), "127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeController{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, recordedRequest{r.Method, r.URL.RequestURI(), stableBody(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.addr = strings.TrimPrefix(srv.URL, "https://")
	return f
}

// stableBody drops what differs between two runs of the same request: the
// identifiers the client generates.
func stableBody(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	var strip func(any)
	strip = func(n any) {
		switch n := n.(type) {
		case map[string]any:
			for k, child := range n {
				if strings.HasSuffix(k, "request_id") || strings.HasSuffix(k, "idempotency_key") {
					delete(n, k)
					continue
				}
				strip(child)
			}
		case []any:
			for _, child := range n {
				strip(child)
			}
		}
	}
	strip(v)
	out, _ := json.Marshal(v)
	return string(out)
}

// take returns what arrived since the last call.
func (f *fakeController) take() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.seen
	f.seen = nil
	return out
}

// Every action drives its command: running it through the registry sends the same
// requests to the Controller and ends with the same exit code as typing the command.
func TestScenario_TuiViewsActions_ParidadCLI_EjecutaCadaAccion(t *testing.T) {
	host, _, _ := initController(t)
	use(t, host)
	newOrigin(t).clone(t) // task new reads the origin of the working directory
	fake := newFakeController(t, host)
	envFile := filepath.Join(t.TempDir(), "env.yaml")
	if err := os.WriteFile(envFile, []byte("name: dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	type drive struct {
		in  actions.Input
		cli []string
	}
	vals := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	drives := map[string]drive{
		"task.new":              {actions.Input{Values: vals("prompt", "fix it", "name", "n1", "project", "backend", "ref", "main")}, []string{"task", "new", "fix it", "--name=n1", "--project=backend", "--ref=main"}},
		"task.set":              {actions.Input{Target: "t1", Values: vals("name", "n2")}, []string{"task", "set", "t1", "--name=n2"}},
		"task.cancel":           {actions.Input{Target: "t1"}, []string{"task", "cancel", "t1"}},
		"task.complete":         {actions.Input{Target: "t1"}, []string{"task", "complete", "t1"}},
		"task.archive":          {actions.Input{Target: "t1"}, []string{"task", "archive", "t1"}},
		"task.defer":            {actions.Input{Target: "t1"}, []string{"task", "defer", "t1"}},
		"task.resume":           {actions.Input{Target: "t1"}, []string{"task", "resume", "t1"}},
		"task.move":             {actions.Input{Target: "t1", Values: vals("worker", "w1")}, []string{"task", "move", "t1", "w1"}},
		"task.respond":          {actions.Input{Target: "t1", Values: vals("allow", "true")}, []string{"task", "respond", "t1", "--allow"}},
		"task.timeline":         {actions.Input{Target: "t1", Values: vals("limit", "5")}, []string{"task", "timeline", "t1", "--limit=5"}},
		"search":                {actions.Input{Values: vals("query", "drain", "kind", "worker", "limit", "5")}, []string{"search", "drain", "--kind=worker", "--limit=5"}},
		"run.fork":              {actions.Input{Target: "r1"}, []string{"run", "fork", "r1"}},
		"run.promote":           {actions.Input{Target: "r1"}, []string{"run", "promote", "r1"}},
		"run.publish":           {actions.Input{Target: "r1"}, []string{"run", "publish", "r1"}},
		"worker.drain":          {actions.Input{Target: "w1"}, []string{"worker", "drain", "w1"}},
		"worker.undrain":        {actions.Input{Target: "w1"}, []string{"worker", "undrain", "w1"}},
		"worker.remove":         {actions.Input{Target: "w1"}, []string{"worker", "remove", "w1"}},
		"workflow.adopt":        {actions.Input{Target: "wf", Values: vals("harness", "codex", "yes", "true")}, []string{"workflow", "adopt", "wf", "--harness=codex", "--yes=true"}},
		"workflow.commit":       {actions.Input{Target: "wf", Values: vals("harness", "codex", "yes", "true")}, []string{"workflow", "commit", "wf", "--harness=codex", "--yes=true"}},
		"workflow.apply":        {actions.Input{Target: "wf", Values: vals("worker", "w1", "harness", "codex", "detach", "true")}, []string{"workflow", "apply", "wf", "--worker=w1", "--harness=codex", "--detach"}},
		"workflow.rollback":     {actions.Input{Values: vals("worker", "w1", "harness", "codex", "detach", "true")}, []string{"workflow", "rollback", "--worker=w1", "--harness=codex", "--detach"}},
		"workflow.sync":         {actions.Input{Target: "wf", Values: vals("harness", "codex", "detach", "true")}, []string{"workflow", "sync", "wf", "--harness=codex", "--detach"}},
		"project.create":        {actions.Input{Values: vals("name", "p1", "repo", "file:///repo")}, []string{"project", "create", "p1", "--repo=file:///repo"}},
		"project.set":           {actions.Input{Target: "p1", Values: vals("default-branch", "dev")}, []string{"project", "set", "p1", "--default-branch=dev"}},
		"project.secrets.allow": {actions.Input{Target: "p1", Values: vals("ref", "env:TOKEN")}, []string{"project", "secrets", "allow", "p1", "env:TOKEN"}},
		"project.secrets.deny":  {actions.Input{Target: "p1", Values: vals("ref", "env:TOKEN")}, []string{"project", "secrets", "deny", "p1", "env:TOKEN"}},
		"environment.apply":     {actions.Input{Values: vals("file", envFile)}, []string{"environment", "apply", "--file=" + envFile}},
		"run.attach":            {actions.Input{Target: "r1"}, []string{"attach", "r1"}},
	}

	opts := cli.Options{Terminal: &spyTerminal{}}
	runner := actions.Runner{Options: opts}
	runCLI := func(args []string) int {
		var out, errOut bytes.Buffer
		return cli.RunContextWith(t.Context(), opts, append(args, "--json", "--controller="+fake.addr), strings.NewReader(""), &out, &errOut)
	}

	for _, d := range actions.All() {
		c, ok := drives[d.ID]
		if !ok {
			t.Errorf("%s: no case drives it", d.ID)
			continue
		}
		c.in.Controller = fake.addr

		var gotExit int
		if d.Terminal {
			_, failure := runner.Attach(t.Context(), d.ID, c.in)
			if failure != nil {
				gotExit = failure.ExitCode
			}
		} else if res := runner.Run(t.Context(), d.ID, c.in); res.Failure != nil {
			gotExit = res.Failure.ExitCode
		}
		viaAction := fake.take()

		wantExit := runCLI(c.cli)
		viaCLI := fake.take()

		if !slices.Equal(viaAction, viaCLI) {
			t.Errorf("%s: the action sent %v, the command %v", d.ID, viaAction, viaCLI)
		}
		if gotExit != wantExit {
			t.Errorf("%s: the action exits %d, the command %d", d.ID, gotExit, wantExit)
		}
		if gotExit == int(domain.ExitCode(domain.CodeInvalidRequest)) {
			t.Errorf("%s: the command refused the inputs of the action as a usage error", d.ID)
		}
		if len(viaAction) == 0 && d.ID != "workflow.adopt" && d.ID != "workflow.commit" && d.ID != "environment.apply" {
			t.Errorf("%s: it never reached the Controller", d.ID)
		}
	}
	for id := range drives {
		if _, ok := actions.Lookup(id); !ok {
			t.Errorf("case %s drives no action", id)
		}
	}
}
