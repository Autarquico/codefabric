//go:build integration

package api_test

import (
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// relayFixture is a Controller with a git relay and a Worker that holds a Run
// it has already finished and sealed to a real commit.
type relayFixture struct {
	*schedulingFixture
	work   string // a working repository: the Worker's
	commit string // what it sealed
	seq    uint64
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	gittest.Require(t)
	self, err := os.Executable()
	must(t, err)
	rel, err := relay.Open(t.Context(), relay.Config{
		Dir: t.TempDir(), Log: nil, HookArgv: []string{"env", asCF + "=1", self, "relay", "hook", "pre-receive"},
	})
	must(t, err)
	f := &relayFixture{schedulingFixture: &schedulingFixture{}}
	f.testController = buildController(t, time.Now(), nil, rel, func(c *api.Config) { f.schedulingFixture.work = c.Work })
	ctx := t.Context()
	f.alpha, f.alphaD = f.enrollRealWorker("alpha")
	f.beta, f.betaD = f.enrollRealWorker("beta")
	for _, w := range []*client.Client{f.alpha, f.beta} {
		_, err := w.Heartbeat(ctx, heartbeat(newEpoch()))
		must(t, err)
	}
	_, err = f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)
	created, err := f.owner.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA})
	must(t, err)
	f.run = *created.Run
	_, err = f.schedulingFixture.work.Scheduler.Pass(ctx)
	must(t, err)
	page, err := f.alpha.Assignments(ctx, 0, 0, 0)
	must(t, err)
	f.assign = *page.Commands[0].Assign
	_, err = f.alpha.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
	must(t, err)

	f.work = gittest.InitRepo(t)
	f.commit = gittest.Run(t, f.work, "rev-parse", "HEAD")
	return f
}

var epoch = newEpoch()

func (f *relayFixture) report(kind v1.RunReportKind, mutate ...func(*v1.RunReportEntry)) v1.RunReportResponse {
	f.t.Helper()
	f.seq++
	code := 0
	e := v1.RunReportEntry{WorkerEpoch: epoch, WorkerSeq: f.seq, LeaseEpoch: f.assign.Epoch, Kind: kind, At: time.Now()}
	switch kind {
	case v1.ReportStarted:
		e.Handle = &v1.RuntimeHandle{Runtime: "tmux", Ref: "x"}
	case v1.ReportExited:
		e.ExitCode, e.Cause = &code, v1.ExitCauseExit
	}
	for _, m := range mutate {
		m(&e)
	}
	out, err := f.alpha.ReportRun(f.t.Context(), f.run.ID, v1.RunReportRequest{Entries: []v1.RunReportEntry{e}})
	must(f.t, err)
	if r := out.Outcomes[0]; r.Result != v1.ReportApplied {
		f.t.Fatalf("%s: %+v", kind, r)
	}
	return out
}

func (f *relayFixture) seal() {
	f.report(v1.ReportStarted)
	f.report(v1.ReportExited)
	f.report(v1.ReportStopped)
	f.report(v1.ReportSealed, func(e *v1.RunReportEntry) { e.ResultCommit = f.commit })
}

// git runs git as the Worker: its certificate, the pinned CA and the token in
// the header, all by environment, as the Worker daemon does.
func (f *relayFixture) git(c *client.Client, token string, args ...string) (string, error) {
	f.t.Helper()
	files := c.TLSFiles()
	env := []string{
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=http.sslCert", "GIT_CONFIG_VALUE_0=" + files.Cert,
		"GIT_CONFIG_KEY_1=http.sslKey", "GIT_CONFIG_VALUE_1=" + files.Key,
		"GIT_CONFIG_KEY_2=http.sslCAInfo", "GIT_CONFIG_VALUE_2=" + files.CA,
		"GIT_CONFIG_KEY_3=http.extraHeader", "GIT_CONFIG_VALUE_3=Authorization: Bearer " + token,
	}
	return gittest.TryEnv(f.t, f.work, env, args...)
}

func TestScenario_GitRelayPublication_RefsProtegidas(t *testing.T) {
	f := newRelayFixture(t)
	f.seal()
	ctx := t.Context()
	tok, err := f.alpha.RelayToken(ctx, f.run.ID, f.assign.Epoch)
	must(t, err)
	if !tok.Write {
		t.Fatal("a sealed Run got no write token")
	}
	remote := f.alpha.RelayURL(tok.Path)
	run, err := f.owner.Run(ctx, f.run.ID)
	must(t, err)

	other := gittest.Commit(t, f.work, "other.txt", "x\n", "another commit")
	gittest.Run(t, f.work, "tag", "-a", "-m", "t", "v1")
	for name, refspec := range map[string]string{
		"main":                  f.commit + ":refs/heads/main",
		"the branch of another": f.commit + ":refs/heads/cf/other/run-9",
		"a tag":                 "v1:refs/tags/v1",
		"another commit":        other + ":refs/heads/" + run.Branch,
		"a deletion":            ":refs/heads/" + run.Branch,
	} {
		if out, err := f.git(f.alpha, tok.Token, "push", remote, refspec); err == nil {
			t.Errorf("%s was accepted:\n%s", name, out)
		}
	}
	// Reads need no write; a push without the token is not even started.
	if _, err := f.git(f.alpha, "", "push", remote, f.commit+":refs/heads/"+run.Branch); err == nil {
		t.Error("a push without a token was accepted")
	}

	// The granted push is the one thing that works.
	if out, err := f.git(f.alpha, tok.Token, "push", remote, f.commit+":refs/heads/"+run.Branch); err != nil {
		t.Fatalf("the granted push: %v\n%s", err, out)
	}
	// A force-push over it is refused.
	gittest.Run(t, f.work, "checkout", "--quiet", "-b", "diverged", "HEAD~1")
	diverged := gittest.Commit(t, f.work, "d.txt", "d\n", "diverged")
	if out, err := f.git(f.alpha, tok.Token, "push", "--force", remote, diverged+":refs/heads/"+run.Branch); err == nil {
		t.Errorf("a force-push was accepted:\n%s", out)
	}

	f.report(v1.ReportPublished, func(e *v1.RunReportEntry) { e.ResultCommit = f.commit })
	got, err := f.owner.Run(ctx, f.run.ID)
	must(t, err)
	if got.Publication != domain.PublicationPublished {
		t.Fatalf("publication = %s", got.Publication)
	}
	// After published nobody can write the branch: not even with the same token.
	if out, err := f.git(f.alpha, tok.Token, "push", "--force", remote, diverged+":refs/heads/"+run.Branch); err == nil {
		t.Errorf("the branch of a published Run moved:\n%s", out)
	}
	if strings.Contains(f.logs.String(), tok.Token) {
		t.Error("the token is in the Controller's log")
	}
}

func TestScenario_GitRelayPublication_FetchNoAutorizado(t *testing.T) {
	f := newRelayFixture(t)
	f.seal()
	ctx := t.Context()
	tok, err := f.alpha.RelayToken(ctx, f.run.ID, f.assign.Epoch)
	must(t, err)
	remote := f.alpha.RelayURL(tok.Path)

	// The Worker with the Run reads.
	if out, err := f.git(f.alpha, tok.Token, "ls-remote", remote); err != nil {
		t.Fatalf("ls-remote with its token: %v\n%s", err, out)
	}
	// A Worker with no Runs in the Project: 403, with no token and with another's.
	for name, token := range map[string]string{"no token": "", "another Worker's token": tok.Token} {
		out, err := f.git(f.beta, token, "ls-remote", f.beta.RelayURL(tok.Path))
		if err == nil || !strings.Contains(out, "403") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	// Another Project's URL with this Worker's token.
	other, err := f.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "other", Repo: "/srv/o"})
	must(t, err)
	out, err := f.git(f.alpha, tok.Token, "ls-remote", f.alpha.RelayURL("/v1/relay/"+other.ID+".git"))
	if err == nil || !strings.Contains(out, "403") {
		t.Errorf("another Project: %v\n%s", err, out)
	}
	// Names that are not a Project ID reach nothing.
	for _, path := range []string{"/v1/relay/..%2F..%2Fcontroller.db/info/refs?service=git-upload-pack", "/v1/relay/" + strings.ToLower(tok.Path[len("/v1/relay/"):]) + "/info/refs"} {
		hc := f.httpClientFor(f.alphaD)
		if code, _, _ := f.call(hc, "GET", path, ""); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
}

func TestRelay_AnExpiredTokenAndTheWrongKindOfIdentityAreRefused(t *testing.T) {
	f := newRelayFixture(t)
	f.seal()
	ctx := t.Context()
	tok, err := f.alpha.RelayToken(ctx, f.run.ID, f.assign.Epoch)
	must(t, err)
	remote := f.alpha.RelayURL(tok.Path)
	if out, err := f.git(f.alpha, tok.Token, "ls-remote", remote); err != nil {
		t.Fatalf("before it expires: %v\n%s", err, out)
	}

	// A client (the owner) is not a Worker, token or not.
	out, err := f.git(f.owner, tok.Token, "ls-remote", remote)
	if err == nil || !strings.Contains(out, "403") {
		t.Errorf("a client at the relay: %v\n%s", err, out)
	}

	f.clock.Advance(runs.RelayTokenTTL + time.Minute)
	out, err = f.git(f.alpha, tok.Token, "ls-remote", remote)
	if err == nil || !strings.Contains(out, "403") {
		t.Errorf("an expired token: %v\n%s", err, out)
	}
	fresh, err := f.alpha.RelayToken(ctx, f.run.ID, f.assign.Epoch)
	must(t, err)
	if out, err := f.git(f.alpha, fresh.Token, "ls-remote", remote); err != nil {
		t.Errorf("a new token: %v\n%s", err, out)
	}
}
