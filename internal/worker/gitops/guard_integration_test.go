//go:build integration

package gitops_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"math/big"
	"net/http/cgi" //nolint:gosec // the relay of the tests runs git http-backend; Go is far past 1.6.3
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
)

const (
	secretRef   = "github.personal"
	secretValue = "ghp-guard-sentinel-0123456789" //nolint:gosec // a sentinel of the tests, not a credential
)

// relay is a real git relay over TLS: git http-backend behind a CGI handler,
// with a bare repository that takes pushes.
type relay struct {
	dir  string
	auth gitops.PushAuth
}

func newRelay(t *testing.T) *relay {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "relay.git")
	gittest.Run(t, root, "init", "--bare", "--quiet", dir)
	gittest.Run(t, dir, "config", "http.receivepack", "true")
	srv := httptest.NewTLSServer(&cgi.Handler{
		Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1"},
	})
	t.Cleanup(srv.Close)
	pems := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "w"}, NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return &relay{dir: dir, auth: gitops.PushAuth{
		URL: srv.URL + "/relay.git", Token: "tok-0123456789abcdef-relay",
		CA:   writePEM(t, filepath.Join(pems, "ca.pem"), "CERTIFICATE", srv.Certificate().Raw),
		Cert: writePEM(t, filepath.Join(pems, "cert.pem"), "CERTIFICATE", der),
		Key:  writePEM(t, filepath.Join(pems, "key.pem"), "EC PRIVATE KEY", keyDER),
	}}
}

// holds says whether any object of the relay contains value.
func (rl *relay) holds(t *testing.T, value string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "--batch-all-objects", "--batch") //nolint:gosec // a test helper
	cmd.Dir, cmd.Env = rl.dir, gittest.Env(t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reading the relay: %v", err)
	}
	return bytes.Contains(out, []byte(value))
}

func (rl *relay) refs(t *testing.T) string {
	t.Helper()
	return gittest.Run(t, rl.dir, "for-each-ref")
}

func (r *rig) forbidden(values ...string) gitops.Forbidden {
	f := gitops.Forbidden{Since: r.source}
	for i, v := range values {
		f.Values = append(f.Values, redact.Needle{Ref: []string{secretRef, "api.other"}[i], Value: []byte(v)})
	}
	return f
}

// isBlocked fails unless err is the guard's, naming ref and holding no value.
func isBlocked(t *testing.T, err error, ref string) {
	t.Helper()
	var b *gitops.BlockedError
	if !errors.As(err, &b) {
		t.Fatalf("not blocked by the guard: %v", err)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("the error holds the value: %v", err)
	}
	if ref != "" && !strings.Contains(b.Reason, `"`+ref+`"`) {
		t.Errorf("the reason does not name %s: %s", ref, b.Reason)
	}
}

// nothingRecorded fails if the sealing moved a ref.
func (r *rig) nothingRecorded(t *testing.T) {
	t.Helper()
	if _, err := gittest.Try(t, r.bare(), "rev-parse", "--verify", "--quiet", gitops.PrivateRef(run, 1)); err == nil {
		t.Error("a private ref was recorded")
	}
	if tip := gittest.Run(t, r.bare(), "rev-parse", "refs/heads/"+gitops.LocalBranch(run, 1)); tip != r.source {
		t.Errorf("the local branch moved to %s", tip)
	}
}

func TestGuard_AValueAnywhereInTheResultBlocksTheSealing(t *testing.T) {
	big := strings.Repeat("filler line of a large generated file\n", (9<<20)/38) + "token=" + secretValue + "\n"
	for name, setup := range map[string]func(r *rig){
		"a new file":      func(r *rig) { r.write("notes/new.txt", "token="+secretValue+"\n") },
		"a modified file": func(r *rig) { r.write("README", "hello\n"+secretValue+"\n") },
		"a commit of the harness, removed later": func(r *rig) {
			r.write("leak.txt", secretValue)
			gittest.Run(t, r.worktree, "add", "leak.txt")
			gittest.Run(t, r.worktree, "commit", "--quiet", "-m", "add")
			gittest.Run(t, r.worktree, "rm", "--quiet", "leak.txt")
			gittest.Run(t, r.worktree, "commit", "--quiet", "-m", "remove")
		},
		"a commit message": func(r *rig) {
			gittest.Run(t, r.worktree, "commit", "--quiet", "--allow-empty", "-m", "debug "+secretValue)
		},
		"a file over 8 MiB": func(r *rig) { r.write("big.log", big) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.prepare()
			setup(r)
			head := gittest.Run(t, r.worktree, "rev-parse", "HEAD")
			status := gittest.Run(t, r.worktree, "status", "--porcelain=v1")
			_, err := r.g.Seal(t.Context(), r.worktree, r.intent(head), r.forbidden(secretValue))
			isBlocked(t, err, secretRef)
			if _, err := gittest.Try(t, r.bare(), "rev-parse", "--verify", "--quiet", gitops.PrivateRef(run, 1)); err == nil {
				t.Error("a private ref was recorded")
			}
			if after := gittest.Run(t, r.worktree, "status", "--porcelain=v1"); after != status {
				t.Errorf("the worktree changed: %s", after)
			}
		})
	}
}

func TestGuard_ResultsWithoutTheValueAreSealedAsInPhase1(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("work.txt", "real work\n")
	r.write(".env", "TOKEN="+secretValue+"\n") // excluded: never in the result
	plain, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), gitops.Forbidden{})
	if err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, r.bare(), "update-ref", "-d", gitops.PrivateRef(run, 1))
	guarded, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), r.forbidden(secretValue, "api-other-value-9876543210"))
	if err != nil || guarded.Commit != plain.Commit || strings.Join(guarded.Excluded, ",") != strings.Join(plain.Excluded, ",") {
		t.Fatalf("guarded = %+v (%v), plain = %+v", guarded, err, plain)
	}
	rl := newRelay(t)
	if err := r.g.Push(t.Context(), project, guarded.Commit, branch, rl.auth, r.forbidden(secretValue)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := gittest.Run(t, rl.dir, "rev-parse", "refs/heads/"+branch); got != guarded.Commit {
		t.Errorf("relay branch = %s", got)
	}
}

func TestGuard_APrivateRefOfAnEarlierLifeIsCheckedToo(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("new.txt", secretValue)
	if _, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), gitops.Forbidden{}); err != nil {
		t.Fatal(err)
	}
	_, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), r.forbidden(secretValue))
	isBlocked(t, err, secretRef)
}

// TestGuard_NothingWithTheValueReachesTheRelay: a push of a result that holds
// the value, to the branch or to the ref of an orphan, sends nothing; without
// the guard (the control) the same push does put the value in the relay.
func TestGuard_NothingWithTheValueReachesTheRelay(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("leak.txt", "x "+secretValue+" y\n")
	gittest.Run(t, r.worktree, "add", "leak.txt")
	gittest.Run(t, r.worktree, "commit", "--quiet", "-m", "harness work")
	commit := gittest.Run(t, r.worktree, "rev-parse", "HEAD")
	rl := newRelay(t)

	isBlocked(t, r.g.Push(t.Context(), project, commit, branch, rl.auth, r.forbidden(secretValue)), secretRef)
	orphan := "refs/cf/orphans/" + run + "-e1"
	isBlocked(t, r.g.PushRef(t.Context(), project, commit, orphan, rl.auth, r.forbidden(secretValue)), secretRef)
	if rl.holds(t, secretValue) || rl.refs(t) != "" {
		t.Fatalf("the relay got something: %s", rl.refs(t))
	}

	if err := r.g.PushRef(t.Context(), project, commit, orphan, rl.auth, gitops.Forbidden{}); err != nil {
		t.Fatalf("control push: %v", err)
	}
	if !rl.holds(t, secretValue) {
		t.Fatal("the control push did not put the value in the relay: the check of the relay proves nothing")
	}
}

func TestGuard_WhatCannotBeCheckedIsNotPublished(t *testing.T) {
	gittest.Require(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		catFile string
		timeout time.Duration
		reason  string
	}{
		"git fails":         {catFile: "echo boom >&2; exit 1", timeout: time.Minute, reason: "cannot be checked"},
		"the deadline":      {catFile: "sleep 30", timeout: time.Second, reason: "took too long"},
		"a truncated batch": {catFile: "printf '%s blob 999\\nabc' 0000000000000000000000000000000000000000", timeout: time.Minute, reason: "cannot be checked"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "git")
			body := "#!/bin/sh\ncase \"$1 $2\" in\n'cat-file --batch') " + c.catFile + " ;;\n*) exec '" + realGit + "' \"$@\" ;;\nesac\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // a test script
				t.Fatal(err)
			}
			r := newRig(t)
			g, err := gitops.New(t.Context(), gitops.Options{Paths: r.paths, Log: slog.New(slog.DiscardHandler), GitPath: script, GuardTimeout: c.timeout})
			if err != nil {
				t.Fatal(err)
			}
			r.g = g
			r.prepare()
			r.write("work.txt", "no secret here\n")
			start := time.Now()
			_, err = r.g.Seal(t.Context(), r.worktree, r.intent(r.source), r.forbidden(secretValue))
			isBlocked(t, err, "")
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("reason = %v, want %q", err, c.reason)
			}
			if took := time.Since(start); took > 20*time.Second {
				t.Errorf("the guard took %s", took)
			}
			r.nothingRecorded(t)
			rl := newRelay(t)
			isBlocked(t, r.g.Push(t.Context(), project, r.source, branch, rl.auth, r.forbidden(secretValue)), "")
			if rl.refs(t) != "" {
				t.Error("something was pushed")
			}
		})
	}
}

func TestGuard_ASourceToCompareWithIsRequired(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("work.txt", "w\n")
	f := r.forbidden(secretValue)
	f.Since = ""
	_, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), f)
	isBlocked(t, err, "")
	r.nothingRecorded(t)
}
