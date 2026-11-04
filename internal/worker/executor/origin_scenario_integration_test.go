//go:build integration && (linux || darwin)

package executor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/testutil/gitorigin"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// These scenarios run the executor of a Worker over the real gitops, the real
// git, the real script of the Worker and the real `cf worker git-askpass`
// (the test binary stands for cf, see TestMain), against a smart-HTTP origin
// over TLS that wants Basic credentials. Only the Controller, the Runtime
// and the secrets catalog are fakes. The run, state and data directories have
// spaces in their path.

const (
	scenarioProject = "project_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	scenarioUser    = secrets.DefaultUser
	scenarioToken   = "tk/+=abc123XYZ_origin-secret" //nolint:gosec // a sentinel of the tests, not a credential
)

type originRig struct {
	*fixture
	origin *gitorigin.Origin
	commit string
	home   string
	logs   *syncLog
	user   string // the Basic user of the credential
	repo   string // the origin the Project froze
	other  string // another repository of the same host
}

// newOriginRig is a Worker whose Project has origin_secret allowed (the secret
// is held for it) and a private repository behind the origin. wrapGit, if
// set, is a script that stands for the git binary: it is given the real one.
func newOriginRig(t *testing.T, wrapGit func(realGit string, r *originRig) string) *originRig {
	t.Helper()
	return newOriginRigAs(t, scenarioUser, wrapGit)
}

// newOriginRigAs is newOriginRig for a credential whose user is user.
func newOriginRigAs(t *testing.T, user string, wrapGit func(realGit string, r *originRig) string) *originRig {
	t.Helper()
	gittest.Require(t)
	// Every directory of the test is under a path with spaces and a comma.
	// t.TempDir keeps one base per test, made on the first call: TMPDIR has to
	// be set before it.
	spaced, err := os.MkdirTemp("", "cf run dir, here ")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(spaced) })
	t.Setenv("TMPDIR", spaced)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("NO_PROXY", "127.0.0.1")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
	src, other := gittest.InitRepo(t), gittest.InitRepo(t)
	commit := gittest.Commit(t, src, "private.txt", "only with the credential\n", "private")
	r := &originRig{home: home, commit: commit, logs: &syncLog{}, user: user}
	r.origin = gitorigin.New(t, map[string]string{"org/repo.git": src, "org/other.git": other}, user, scenarioToken)
	r.repo, r.other = r.origin.URL+"/org/repo.git", r.origin.URL+"/org/other.git"
	log := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newFakeStore(map[string]string{originRef: scenarioToken})
	store.scope(originRef, scenarioProject)
	if user != scenarioUser {
		e := store.entries[originRef]
		e.user = user
		store.entries[originRef] = e
	}
	r.fixture = newFixture(t, func(o *Options) {
		script, err := EnsureAskpassScript(o.Paths, []string{"/usr/bin/env", asWrapper + "=1", mustExecutable(t), "worker", "git-askpass"})
		if err != nil {
			t.Fatal(err)
		}
		gitPath := ""
		if wrapGit != nil {
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			gitPath = filepath.Join(t.TempDir(), "git")
			if err := os.WriteFile(gitPath, []byte(wrapGit(realGit, r)), 0o700); err != nil { //nolint:gosec // a git wrapper of the test
				t.Fatal(err)
			}
		}
		g, err := gitops.New(t.Context(), gitops.Options{Paths: o.Paths, Clock: gitorigin.NoWait{}, Log: log, GitPath: gitPath})
		if err != nil {
			t.Fatal(err)
		}
		gitops.TrustCA(g, r.origin.CA)
		o.Secrets, o.Git, o.AskpassScript, o.Log = store, g, script, log
		o.RedactPipeArgv = []string{"/cf", "worker", "redact-pipe"}
	})
	if !strings.Contains(r.paths.DataDir, " ") {
		t.Fatalf("the data directory has no space in its path: %s", r.paths.DataDir)
	}
	return r
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func originSpecAt(url string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"schema": 2, "prompt": "echo hi", "mode": "headless",
		"source":       map[string]any{"ref": "main"},
		"requirements": map[string]any{"harness": "command"},
		"origin":       map[string]any{"secret": originRef, "url": url},
		"repo_config":  "deny",
		"origins":      map[string]any{"resources": "project"},
	})
	return raw
}

// assignAt assigns a Run of the Project to the repository repo, whose origin
// froze the URL frozen.
func (r *originRig) assignAt(repo, frozen string) state.Key {
	r.t.Helper()
	k := r.assign(originSpecAt(frozen), func(a *v1.AssignCommand) { a.Repo, a.SourceCommit, a.ProjectID = repo, r.commit, scenarioProject })
	r.e.decideOnce(r.t.Context())
	return k
}

// settle steps the Run until it stops or is running, moving the clock past
// the retries of a prepare.
func (r *originRig) settle(k state.Key) {
	r.t.Helper()
	for range 12 {
		r.e.step(r.t.Context(), k)
		if p := r.run(k).Phase; p != state.PhaseAccepted {
			return
		}
		r.clock.Advance(time.Minute)
	}
}

func (r *originRig) worktree(k state.Key) string {
	return r.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch)
}

func tokenForms(basicUser string) map[string]string {
	user := basicUser + ":" + scenarioToken
	return map[string]string{
		"token":                 scenarioToken,
		"query-escaped":         url.QueryEscape(scenarioToken),
		"path-escaped":          url.PathEscape(scenarioToken),
		"base64 user:token":     base64.StdEncoding.EncodeToString([]byte(user)),
		"base64 raw user:token": base64.RawStdEncoding.EncodeToString([]byte(user)),
		"base64 url user:token": base64.URLEncoding.EncodeToString([]byte(user)),
	}
}

func noTokenIn(t *testing.T, what, text string, users ...string) {
	t.Helper()
	for _, user := range append([]string{scenarioUser}, users...) {
		for name, form := range tokenForms(user) {
			if strings.Contains(text, form) {
				t.Errorf("%s holds the credential (%s, user %s):\n%s", what, name, user, text)
			}
		}
	}
}

// noTokenAnywhere checks the logs, the reports, the state of the Run, what the
// Runtime was asked to start, and every file of the Worker apart from the
// delivery directories, which are where the credential is meant to be.
func (r *originRig) noTokenAnywhere(k state.Key, errs ...error) {
	r.t.Helper()
	noTokenIn(r.t, "the log", r.logs.String(), r.user)
	for _, err := range errs {
		if err != nil {
			noTokenIn(r.t, "an error", err.Error(), r.user)
		}
	}
	reports, _ := json.Marshal(r.reports(k))
	noTokenIn(r.t, "the reports", string(reports), r.user)
	st, _ := json.Marshal(r.run(k))
	noTokenIn(r.t, "the state of the Run", string(st), r.user)
	r.rt.mu.Lock()
	starts, _ := json.Marshal(r.rt.starts)
	r.rt.mu.Unlock()
	noTokenIn(r.t, "what the Runtime was asked to start", string(starts), r.user)
	root := filepath.Dir(r.paths.ConfigDir)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir() && (strings.HasPrefix(d.Name(), "secrets") || d.Name() == ".git" && strings.HasPrefix(path, r.worktree(k))):
			return filepath.SkipDir
		case d.IsDir():
			return nil
		}
		raw, _ := os.ReadFile(path) //nolint:gosec // a file of the test
		noTokenIn(r.t, path, string(raw), r.user)
		return nil
	})
}

func (r *originRig) askpassDenied(k state.Key) {
	r.t.Helper()
	if _, err := secretjit.AskpassReader(r.runDir(k))(originRef); !errors.Is(err, secretjit.ErrNoAccess) {
		r.t.Errorf("askpass reader: %v, want no access", err)
	}
	if _, err := secretjit.AskpassGrant(r.runDir(k)); !errors.Is(err, secretjit.ErrNoAccess) {
		r.t.Errorf("askpass grant: %v, want no access", err)
	}
	if _, err := secretjit.Resolver(r.runDir(k))(originRef); !errors.Is(err, secretjit.ErrNoAccess) {
		r.t.Errorf("the harness could read the origin credential: %v", err)
	}
}

func (r *originRig) prepareFailedMessage(k state.Key) string {
	r.t.Helper()
	got := r.reports(k)
	if kinds := kinds(got); !slices.Contains(kinds, v1.ReportPrepareFailed) {
		r.t.Fatalf("reports = %v, want a prepare_failed", kinds)
	}
	for _, e := range got {
		if e.Kind == v1.ReportPrepareFailed {
			return e.Message
		}
	}
	return ""
}

func TestScenario_OriginAskpass_RepoPrivado(t *testing.T) {
	r := newOriginRig(t, nil)
	record := gitorigin.HostileHome(t, r.home, r.origin)
	k := r.assignAt(r.repo, r.repo)

	r.settle(k)

	if got := r.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running; reports %v\nlog:\n%s", got, r.reports(k), r.logs)
	}
	if head := gittest.Run(t, r.worktree(k), "rev-parse", "HEAD"); head != r.commit {
		t.Errorf("HEAD = %s, want the private commit %s", head, r.commit)
	}
	if _, err := os.Stat(filepath.Join(r.worktree(k), "private.txt")); err != nil {
		t.Errorf("the private file is not in the worktree: %v", err)
	}
	if n := r.origin.AuthorizedFor("/org/repo.git"); n == 0 {
		t.Errorf("the origin saw no authorized request: %v", r.origin.Saw())
	}
	if _, err := os.Stat(record); err == nil {
		t.Error("a credential helper of the hostile HOME was invoked")
	}
	r.askpassDenied(k)
	if needles, err := secretjit.Load(r.runDir(k)); err != nil || len(needles) != 1 {
		t.Errorf("the needles are not retained after the fetch: %v, %v", needles, err)
	}
	var l execwrap.Launch
	if ok, err := execwrap.Read(r.runDir(k), execwrap.LaunchFile, &l); err != nil || (ok && !l.RedactOutput) {
		t.Errorf("launch = %+v, %v", l, err)
	}
	r.noTokenAnywhere(k)
	if kinds := kinds(r.reports(k)); slices.Contains(kinds, v1.ReportPrepareFailed) {
		t.Errorf("reports = %v", kinds)
	}
}

func TestScenario_OriginAskpass_DestinoNoAutorizado(t *testing.T) {
	failed := func(t *testing.T, r *originRig, k state.Key) {
		t.Helper()
		r.settle(k)
		if got := r.run(k).Phase; got != state.PhaseStopped {
			t.Fatalf("phase = %s, want stopped; reports %v\nlog:\n%s", got, kinds(r.reports(k)), r.logs)
		}
		noTokenIn(t, "the prepare_failed message", r.prepareFailedMessage(k), r.user)
		if !strings.Contains(r.logs.String(), "fetching the source of a Run failed") {
			t.Errorf("the Run did not fail at the fetch:\n%s", r.logs)
		}
		r.askpassDenied(k)
		r.noTokenAnywhere(k)
		if n := r.rt.startCount(); n != 0 {
			t.Errorf("the harness started %d times", n)
		}
	}
	t.Run("the origin redirects to another host", func(t *testing.T) {
		r := newOriginRig(t, nil)
		elsewhere := gitorigin.New(t, map[string]string{"org/repo.git": gittest.InitRepo(t)}, scenarioUser, scenarioToken)
		r.origin.RedirectTo(elsewhere.URL)
		k := r.assignAt(r.repo, r.repo)
		failed(t, r, k)
		if n := r.origin.AuthorizedFor("/org/repo.git"); n == 0 {
			t.Error("the origin never asked git to leave: the redirect was not exercised")
		}
		if got := elsewhere.Saw(); len(got) != 0 {
			t.Errorf("the other host was reached: %v", got)
		}
	})
	t.Run("git asks for another repository of the same host", func(t *testing.T) {
		// A git that rewrites the repository of the Run to another one: its
		// prompt is for a path the credential is not for.
		r := newOriginRig(t, func(realGit string, r *originRig) string {
			return fmt.Sprintf("#!/bin/sh\nexec '%s' -c 'url.%s.insteadOf=%s' \"$@\"\n", realGit, r.other, r.repo)
		})
		k := r.assignAt(r.repo, r.repo)
		failed(t, r, k)
		if !slices.ContainsFunc(r.origin.Saw(), func(s gitorigin.Seen) bool { return strings.HasPrefix(s.Path, "/org/other.git") }) {
			t.Error("git never asked the origin for the other repository: the prompt was not exercised")
		}
		if n := r.origin.AuthorizedFor("/org/other.git"); n != 0 {
			t.Errorf("the credential reached another repository %d times", n)
		}
	})
	t.Run("the assignment names a repository other than the frozen origin", func(t *testing.T) {
		r := newOriginRig(t, nil)
		k := r.assignAt(r.other, r.repo)
		if got := r.run(k).Phase; got == state.PhaseAccepted || got == state.PhaseRunning {
			t.Fatalf("phase = %s: a Run for another repository was taken", got)
		}
		if got := r.origin.Saw(); len(got) != 0 {
			t.Errorf("the origin was reached: %v", got)
		}
		if n := len(r.ctrl.grantRequests()); n != 0 {
			t.Errorf("the credential was granted %d times", n)
		}
		r.noTokenAnywhere(k)
	})
}

func TestScenario_OriginAskpass_ErrorDeFetch(t *testing.T) {
	user := scenarioUser + ":" + scenarioToken
	r := newOriginRig(t, func(realGit string, _ *originRig) string {
		// A git whose fetch fails the way some do: the error shows the URL it
		// was given with the credential in it, the header, and the encodings.
		return fmt.Sprintf(`#!/bin/sh
case "$1" in fetch)
echo "fatal: unable to access 'https://%s@example.com/o/r.git/': 403" >&2
echo "fatal: token %s encoded %s header Authorization: Basic %s" >&2
echo "out: %s"
exit 128;;
esac
exec '%s' "$@"
`, user, scenarioToken, url.QueryEscape(scenarioToken), base64.StdEncoding.EncodeToString([]byte(user)), scenarioToken, realGit)
	})
	k := r.assignAt(r.repo, r.repo)
	r.settle(k)

	if got := r.run(k).Phase; got != state.PhaseStopped {
		t.Fatalf("phase = %s, want stopped; reports %v", got, kinds(r.reports(k)))
	}
	marker := "[REDACTED:" + originRef + "]"
	// gitops reports a failed fetch with the tail of what git said, redacted.
	msg := r.prepareFailedMessage(k)
	if !strings.Contains(msg, marker) {
		t.Errorf("the prepare_failed message does not show %s:\n%s", marker, msg)
	}
	noTokenIn(t, "the prepare_failed message", msg)
	if !strings.Contains(r.logs.String(), marker) {
		t.Errorf("the log does not show %s:\n%s", marker, r.logs)
	}
	r.noTokenAnywhere(k)
	r.askpassDenied(k)
	// A Run that fails at prepare never writes output.log: the git output of the
	// scenario is the prepare_failed message checked above.
	if _, err := os.Stat(filepath.Join(r.runDir(k), execwrap.OutputLog)); err == nil {
		t.Error("a Run that failed at prepare has an output.log")
	}
}

// A credential with a user of its own: git authenticates as it against the
// TLS origin, and a failed fetch that echoes the Basic header of that user is
// redacted of it.
func TestScenario_OriginAskpass_CustomUser(t *testing.T) {
	const user = "deploy-bot"
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + scenarioToken))
	t.Run("the fetch authenticates as the user", func(t *testing.T) {
		r := newOriginRigAs(t, user, nil)
		k := r.assignAt(r.repo, r.repo)
		r.settle(k)
		if got := r.run(k).Phase; got != state.PhaseRunning {
			t.Fatalf("phase = %s, want running; reports %v\nlog:\n%s", got, kinds(r.reports(k)), r.logs)
		}
		if n := r.origin.AuthorizedFor("/org/repo.git"); n == 0 {
			t.Errorf("the origin saw no request authorized as %s: %v", user, r.origin.Saw())
		}
		if needles, err := secretjit.Load(r.runDir(k)); err != nil || len(needles) != 1 || needles[0].User != user {
			t.Errorf("the retained needles = %+v, %v: want one with user %s", needles, err, user)
		}
		r.askpassDenied(k)
		r.noTokenAnywhere(k)
	})
	t.Run("a failed fetch that shows the Basic header is redacted", func(t *testing.T) {
		r := newOriginRigAs(t, user, func(realGit string, _ *originRig) string {
			return fmt.Sprintf(`#!/bin/sh
case "$1" in fetch)
echo "fatal: header Authorization: Basic %s" >&2
exit 128;;
esac
exec '%s' "$@"
`, basic, realGit)
		})
		k := r.assignAt(r.repo, r.repo)
		r.settle(k)
		if got := r.run(k).Phase; got != state.PhaseStopped {
			t.Fatalf("phase = %s, want stopped; reports %v", got, kinds(r.reports(k)))
		}
		msg := r.prepareFailedMessage(k)
		if !strings.Contains(msg, "[REDACTED:"+originRef+"]") || strings.Contains(msg, basic) {
			t.Errorf("the prepare_failed message does not redact the Basic form of %s:\n%s", user, msg)
		}
		r.noTokenAnywhere(k)
	})
}
