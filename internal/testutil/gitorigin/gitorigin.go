// Package gitorigin is a git origin for tests that need a credential: a
// smart-HTTP server over TLS that answers every request without the Basic
// credentials it was built with with a 401, and serves bare repositories to
// those that carry them. It is imported by tests only.
package gitorigin

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/cgi" //nolint:gosec // the origin of the tests runs git http-backend; Go is far past 1.6.3
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// Seen is a request the origin took.
type Seen struct {
	Path       string
	Authorized bool
}

// Origin is the server. Its URL is https://127.0.0.1:<port>; a repository
// "org/repo.git" of New is at URL+"/org/repo.git".
type Origin struct {
	URL string
	// CA is a PEM file with the certificate the server presents.
	CA string

	srv      *httptest.Server
	mu       sync.Mutex
	requests []Seen
	redirect string
}

// New serves a bare clone of each source repository (by its path under the
// root) to the requests that carry user and token as Basic credentials.
func New(t *testing.T, repos map[string]string, user, token string) *Origin {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for name, src := range repos {
		dst := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, root, "clone", "--bare", "--quiet", src, dst)
	}
	backend := &cgi.Handler{
		Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1"},
	}
	o := &Origin{}
	o.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		authorized := ok && u == user && p == token
		o.mu.Lock()
		o.requests = append(o.requests, Seen{Path: req.URL.Path, Authorized: authorized})
		redirect := o.redirect
		o.mu.Unlock()
		switch {
		case !authorized:
			w.Header().Set("WWW-Authenticate", `Basic realm="origin"`)
			http.Error(w, "authentication needed", http.StatusUnauthorized)
		case redirect != "":
			http.Redirect(w, req, redirect+req.URL.RequestURI(), http.StatusTemporaryRedirect) //nolint:gosec // the redirect under test
		default:
			backend.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(o.srv.Close)
	o.URL = o.srv.URL
	o.CA = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: o.srv.Certificate().Raw})
	if err := os.WriteFile(o.CA, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return o
}

// RedirectTo makes the origin, once the credentials are right, answer with a
// redirect to the same path under base. Empty base turns it off.
func (o *Origin) RedirectTo(base string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.redirect = base
}

// Saw is every request taken so far.
func (o *Origin) Saw() []Seen {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Seen(nil), o.requests...)
}

// AuthorizedFor counts the requests with the right credentials whose path
// starts with prefix.
func (o *Origin) AuthorizedFor(prefix string) int {
	n := 0
	for _, r := range o.Saw() {
		if r.Authorized && strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

// NoWait is a clock whose backoffs are over at once: the failures under test
// are retried without the seconds.
type NoWait struct{ clock.Real }

type fired struct{ c chan time.Time }

func (f fired) C() <-chan time.Time { return f.c }
func (fired) Stop()                 {}

// NewTimer is a timer that has already fired.
func (NoWait) NewTimer(time.Duration) clock.Timer {
	c := make(chan time.Time, 1)
	c <- time.Now()
	return fired{c}
}

// HostileHome fills home with a git configuration that would leak or
// weaken any git that reads it: a credential helper that records every call
// (and answers), a rewrite of the origin to another host, no TLS
// verification for it, an extra header, and a core.askPass. It returns the
// file the helper appends to. The control: it first checks that a git that is
// not isolated does call the helper, so that "0 calls" later proves
// something, and removes the record.
func HostileHome(t *testing.T, home string, o *Origin) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "helper-calls")
	helper := fmt.Sprintf("[credential]\n\thelper = \"!f() { echo called >> '%s'; echo username=x; echo password=y; }; f\"\n", record)
	hostile := helper + fmt.Sprintf("[url \"https://evil.invalid/\"]\n\tinsteadOf = %s/\n[http]\n\tsslVerify = false\n"+
		"[http \"%s/\"]\n\tsslVerify = false\n\textraHeader = X-Hostile: 1\n[core]\n\taskPass = /bin/false\n", o.URL, o.URL)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "git", "config"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	helperOnly := filepath.Join(t.TempDir(), "helper-only")
	if err := os.WriteFile(helperOnly, []byte(helper), 0o600); err != nil {
		t.Fatal(err)
	}
	ctl := exec.CommandContext(t.Context(), "git", "ls-remote", o.URL+"/org/repo.git") //nolint:gosec // a test
	ctl.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_GLOBAL=" + helperOnly, "GIT_TERMINAL_PROMPT=0", "GIT_SSL_CAINFO=" + o.CA}
	_ = ctl.Run()
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the recording helper is not invoked by an unisolated git: %v", err)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	return record
}
