//go:build integration

package gitops_test

import (
	"net/http"
	"net/http/cgi" //nolint:gosec // a test server of git over HTTP; the advisory is about Go before 1.6.3
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
)

const resumedCheckpoint = "cp_01J0000000000000000000CP01"

// relayServing is the relay of the Controller as a Worker sees it: git over
// HTTPS with a bearer token, serving a bare repository that holds a commit
// nowhere else, at the ref of a checkpoint. It answers 401 to any other token.
func relayServing(t *testing.T) (url, commit string, stub *relayStub) {
	t.Helper()
	work := gittest.InitRepo(t)
	commit = gittest.Commit(t, work, "work.txt", "left by the first Worker\n", "work in progress")
	root := t.TempDir()
	gittest.Run(t, root, "clone", "--bare", "--quiet", work, filepath.Join(root, "p.git"))
	gittest.Run(t, filepath.Join(root, "p.git"), "update-ref", domain.CheckpointRef(resumedCheckpoint), commit)
	gittest.Run(t, filepath.Join(root, "p.git"), "update-ref", domain.SnapshotRef(commit), commit)
	execPath := gittest.Run(t, root, "--exec-path")
	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	stub = &relayStub{}
	stub.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		stub.mu.Lock()
		stub.headers = append(stub.headers, req.Header.Clone())
		stub.mu.Unlock()
		if req.Header.Get("Authorization") != "Bearer "+relayToken {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, req)
	}))
	return stub.srv.URL + "/p.git", commit, stub
}

const relayToken = "tok-relay-0123456789"

func relaySource(url string, stub *relayStub) *gitops.RelaySource {
	return &gitops.RelaySource{
		Ref:  domain.CheckpointRef(resumedCheckpoint),
		Auth: gitops.PushAuth{URL: url, Token: relayToken, Cert: stub.cert, Key: stub.key, CA: stub.ca},
	}
}

func TestScenario_ResumeOtroWorker_PrepareFetchesTheCheckpointFromTheRelayAndNotFromOrigin(t *testing.T) {
	r := newRig(t)
	url, commit, stub := relayServing(t)
	originRefs := gittest.Run(t, r.origin, "for-each-ref")

	spec := r.spec()
	spec.SourceCommit, spec.SourceRef = commit, "main"
	// The origin is not reachable at all: whatever is fetched, is not from there.
	spec.Repo = filepath.Join(t.TempDir(), "gone")
	spec.Relay = relaySource(url, stub)
	if err := r.g.Prepare(t.Context(), spec, r.worktree); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if got := gittest.Run(t, r.worktree, "rev-parse", "HEAD"); got != commit {
		t.Errorf("HEAD = %s, want the checkpoint commit %s", got, commit)
	}
	if got := gittest.Run(t, r.worktree, "rev-parse", "--abbrev-ref", "HEAD"); got != gitops.LocalBranch(run, 1) {
		t.Errorf("branch = %s", got)
	}
	if _, err := gittest.Try(t, r.origin, "cat-file", "-e", commit+"^{commit}"); err == nil {
		t.Error("the checkpoint commit is in the origin")
	}
	if got := gittest.Run(t, r.origin, "for-each-ref"); got != originRefs {
		t.Errorf("the origin changed:\n%s\n--\n%s", originRefs, got)
	}
	if refs := gittest.Run(t, r.bare(), "for-each-ref", "refs/cf/checkpoints"); refs != "" {
		t.Errorf("the checkpoint ref was written in the repository of the Worker: %s", refs)
	}
}

func TestPrepare_RelaySourceNeedsACheckpointRefAndCredentials(t *testing.T) {
	r := newRig(t)
	url, commit, stub := relayServing(t)
	for name, mutate := range map[string]func(*gitops.RelaySource){
		"a branch":       func(s *gitops.RelaySource) { s.Ref = "refs/heads/main" },
		"no token":       func(s *gitops.RelaySource) { s.Auth.Token = "" },
		"no certificate": func(s *gitops.RelaySource) { s.Auth.Cert = "" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := r.spec()
			spec.SourceCommit, spec.Relay = commit, relaySource(url, stub)
			mutate(spec.Relay)
			if err := r.g.Prepare(t.Context(), spec, r.worktree); err == nil {
				t.Error("Prepare accepted it")
			}
		})
	}
}

func TestPrepare_RelayThatLacksTheCommitFails(t *testing.T) {
	r := newRig(t)
	url, _, stub := relayServing(t)
	spec := r.spec()
	// A commit the ref of the checkpoint does not hold.
	spec.SourceCommit, spec.Relay = strings.Repeat("a", 40), relaySource(url, stub)
	err := r.g.Prepare(t.Context(), spec, r.worktree)
	if err == nil || !strings.Contains(err.Error(), "does not hold") {
		t.Errorf("Prepare = %v", err)
	}
}

func TestScenario_DirtySnapshots_PrepareFetchesTheSnapshotFromTheRelayAndNotFromOrigin(t *testing.T) {
	r := newRig(t)
	url, commit, stub := relayServing(t)

	spec := r.spec()
	spec.SourceCommit, spec.SourceRef = commit, "main"
	spec.Repo = filepath.Join(t.TempDir(), "gone")
	spec.Relay = relaySource(url, stub)
	spec.Relay.Ref = domain.SnapshotRef(commit)
	if err := r.g.Prepare(t.Context(), spec, r.worktree); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := gittest.Run(t, r.worktree, "rev-parse", "HEAD"); got != commit {
		t.Errorf("HEAD = %s, want the snapshot commit %s", got, commit)
	}
	if _, err := gittest.Try(t, r.origin, "cat-file", "-e", commit+"^{commit}"); err == nil {
		t.Error("the snapshot commit is in the origin")
	}
}
