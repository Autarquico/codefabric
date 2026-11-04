//go:build linux || darwin

package executor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const (
	originRef   = "github.origin"
	originURL   = "https://github.com/acme/app"
	originValue = "ghp-origin-sentinel-0123456789"
)

func originSpec(schema int, url string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"schema": schema, "prompt": "echo hi", "mode": "headless",
		"requirements": map[string]any{"harness": "command"},
		"origin":       map[string]any{"secret": originRef, "url": url},
		"repo_config":  domain.RepoConfigDeny,
		"origins":      map[string]any{"resources": "project"},
	})
	return raw
}

// originRun is an accepted Run with an origin credential over a fake git.
func originRun(t *testing.T, schema int, g *fakeGit) (*fixture, state.Key) {
	t.Helper()
	store := newFakeStore(map[string]string{originRef: originValue})
	f := secretFixture(t, store, func(o *Options) {
		o.Git = g
		o.AskpassScript = "/state/bin/git-askpass"
	})
	k := f.assign(originSpec(schema, originURL), func(a *v1.AssignCommand) { a.Repo = originURL })
	if f.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Fatalf("phase = %s, want accepted", got)
	}
	return f, k
}

func askpassCan(dir string) error {
	_, err := secretjit.AskpassReader(dir)(originRef)
	return err
}

// during is what a fetch finds: the grant, the delivery and the binding.
type during struct {
	granted    bool
	grant      secretjit.Grant
	grantErr   error
	auth       *gitops.OriginAuth
	harnessErr error
}

func (f *fixture) observe(k state.Key, auth *gitops.OriginAuth) during {
	d := during{auth: auth, granted: len(f.ctrl.grantRequests()) > 0}
	d.grant, d.grantErr = secretjit.AskpassGrant(f.runDir(k))
	_, d.harnessErr = secretjit.Resolver(f.runDir(k))(originRef)
	return d
}

func TestOriginCredentialIsGrantedDeliveredAndBoundBeforeTheFetchAndRevokedAfter(t *testing.T) {
	var seen during
	var k state.Key
	var f *fixture
	g := &fakeGit{onPrepare: func(s gitops.PrepareSpec, _ string) error {
		seen = f.observe(k, s.Auth)
		return nil
	}}
	f, k = originRun(t, 2, g)
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	if !seen.granted || seen.grantErr != nil || seen.grant.Value != originValue || seen.grant.Origin != originURL || seen.grant.User != secrets.DefaultUser {
		t.Errorf("at the fetch: %+v", seen)
	}
	if !errors.Is(seen.harnessErr, secretjit.ErrNoAccess) {
		t.Errorf("the harness could read the origin credential: %v", seen.harnessErr)
	}
	if seen.auth == nil || seen.auth.Askpass != "/state/bin/git-askpass" || seen.auth.RunDir != f.runDir(k) ||
		len(seen.auth.Needles) != 1 || seen.auth.Needles[0].User != secrets.DefaultUser {
		t.Errorf("auth = %+v", seen.auth)
	}
	if reqs := f.ctrl.grantRequests(); len(reqs) != 1 || !slices.Equal(reqs[0].Refs, []string{originRef}) {
		t.Errorf("grants = %+v", reqs)
	}
	if err := askpassCan(f.runDir(k)); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("askpass after the fetch: %v", err)
	}
	if needles, err := secretjit.Load(f.runDir(k)); err != nil || len(needles) != 1 {
		t.Errorf("retention = %v, %v", needles, err)
	}
	var l execwrap.Launch
	if ok, err := execwrap.Read(f.runDir(k), execwrap.LaunchFile, &l); !ok || err != nil || !l.RedactOutput {
		t.Errorf("launch = %+v, %v: an origin-only Run is not redacted", l, err)
	}
}

func TestOriginCredentialIsRevokedWhenTheFetchFails(t *testing.T) {
	g := &fakeGit{prepareErr: errors.New("fatal: could not read from " + originValue)}
	f, k := originRun(t, 2, g)
	err := f.e.preparing(t.Context(), f.run(k))
	if err == nil {
		t.Fatal("the fetch failed and the prepare did not")
	}
	if strings.Contains(err.Error(), originValue) || !strings.Contains(err.Error(), "[REDACTED:"+originRef+"]") {
		t.Errorf("error = %q", err)
	}
	if err := askpassCan(f.runDir(k)); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("askpass after a failed fetch: %v", err)
	}
	if _, err := secretjit.Load(f.runDir(k)); err != nil {
		t.Errorf("retention lost: %v", err)
	}
}

func TestOriginCredentialIsGrantedAgainUnderANewAttemptAcrossTheAckWait(t *testing.T) {
	var attempts []uint64
	var seen []during
	var f *fixture
	var k state.Key
	observe := func(auth *gitops.OriginAuth) {
		seen = append(seen, f.observe(k, auth))
		attempts = append(attempts, f.run(k).PrepareAttempt)
	}
	g := &fakeGit{
		onEnsure:  func(s gitops.SourceSpec) error { observe(s.Auth); return nil },
		onPrepare: func(s gitops.PrepareSpec, _ string) error { observe(s.Auth); return nil },
	}
	f, k = originRun(t, 4, g)
	if err := f.e.prepare(t.Context(), f.run(k)); !errors.Is(err, errAckPending) {
		t.Fatalf("first prepare = %v, want the ack wait", err)
	}
	if err := askpassCan(f.runDir(k)); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("askpass while the ack is awaited: %v", err)
	}
	f.confirmReports(k)
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	if got := g.callList(); !slices.Equal(got, []string{"ensure", "prepare"}) {
		t.Fatalf("calls = %v", got)
	}
	if len(seen) != 2 || seen[0].grantErr != nil || seen[1].grantErr != nil || seen[1].grant.Value != originValue {
		t.Fatalf("each fetch finds the credential: %+v", seen)
	}
	if attempts[1] <= attempts[0] {
		t.Errorf("attempts = %v: the second fetch did not run under a new one", attempts)
	}
	if reqs := f.ctrl.grantRequests(); len(reqs) != 2 {
		t.Errorf("grants = %d, want one per attempt", len(reqs))
	}
	if err := askpassCan(f.runDir(k)); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("askpass after the last fetch: %v", err)
	}
}

func TestAnOldAttemptNeverRevokesANewerDelivery(t *testing.T) {
	f, k := originRun(t, 2, &fakeGit{})
	r := f.run(k)
	spec := parseSpec(r.Spec)
	next := func() *originAccess {
		var p *secretPrepare
		return f.e.originOf(r, spec, func() (*secretPrepare, error) {
			var err error
			p, err = f.e.beginSecrets(t.Context(), r)
			return p, err
		})
	}
	old, newer := next(), next()
	if _, err := old.credential(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := newer.credential(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := old.revoke(); err != nil {
		t.Fatalf("the late revoke: %v", err)
	}
	if err := askpassCan(f.runDir(k)); err != nil {
		t.Errorf("the old attempt revoked the newer delivery: %v", err)
	}
	if err := newer.revoke(); err != nil {
		t.Fatal(err)
	}
	if err := askpassCan(f.runDir(k)); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("the owner's revoke: %v", err)
	}
	if _, err := newer.credential(t.Context()); err == nil {
		t.Error("access was given back after the revoke")
	}
}

func TestAnAssignmentThatIsNotTheFrozenOriginIsRefused(t *testing.T) {
	g := &fakeGit{}
	f := secretFixture(t, newFakeStore(map[string]string{originRef: originValue}), func(o *Options) { o.Git = g })
	k := f.assign(originSpec(2, originURL), func(a *v1.AssignCommand) { a.Repo = "https://github.com/acme/other" })
	f.e.decideOnce(t.Context())
	if len(f.ctrl.rejected) != 1 || f.ctrl.rejected[0].Code != domain.CodePolicyDenied || !strings.Contains(f.ctrl.rejected[0].Message, "origin") {
		t.Errorf("rejected = %+v", f.ctrl.rejected)
	}
	// And a Run that got past the decision is stopped by the prepare.
	err := f.e.prepare(t.Context(), f.run(k))
	if err == nil || !strings.Contains(err.Error(), string(domain.CodePolicyDenied)) {
		t.Errorf("prepare = %v", err)
	}
	if got := g.callList(); len(got) != 0 || len(f.ctrl.grantRequests()) != 0 {
		t.Errorf("git %v or grants %v on a mismatching assignment", got, f.ctrl.grantRequests())
	}
}

func TestAnOriginSecretTheWorkerCannotReadFailsWithTheCodedUnavailable(t *testing.T) {
	g := &fakeGit{}
	f, k := originRun(t, 2, g)
	if err := f.store().Delete(t.Context(), originRef); err != nil {
		t.Fatal(err)
	}
	err := f.e.prepare(t.Context(), f.run(k))
	var coded *codedFailure
	if !errors.As(err, &coded) || coded.code != domain.CodeSecretUnavailable {
		t.Fatalf("prepare = %v, want CF_SECRET_UNAVAILABLE", err)
	}
	if len(g.callList()) != 0 {
		t.Errorf("git ran without a credential: %v", g.callList())
	}
}

func TestOriginOnlyRunRetainsItsSecretsForTheRelease(t *testing.T) {
	f, k := originRun(t, 2, &fakeGit{})
	if !f.e.usesSecrets(f.run(k)) {
		t.Error("an origin-only Run is not a Run with secrets")
	}
}

// withOrigin adds the origin of originSpec to a spec that already has secrets.
func withOrigin(t *testing.T, spec []byte, url string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(spec, &m); err != nil {
		t.Fatal(err)
	}
	m["origin"] = map[string]any{"secret": originRef, "url": url}
	m["repo_config"] = domain.RepoConfigDeny
	m["origins"] = map[string]any{"resources": "project"}
	raw, _ := json.Marshal(m)
	return raw
}

func TestAnAttemptWithAnOriginAndHarnessSecretsGivesEachConsumerOnlyItsOwn(t *testing.T) {
	const harnessRef = "github.personal"
	var atFetch struct{ origin, harness error }
	var f *fixture
	var k state.Key
	g := &fakeGit{onPrepare: func(gitops.PrepareSpec, string) error {
		_, atFetch.origin = secretjit.AskpassReader(f.runDir(k))(originRef)
		_, atFetch.harness = secretjit.AskpassReader(f.runDir(k))(harnessRef)
		return nil
	}}
	store := newFakeStore(map[string]string{originRef: originValue, harnessRef: ghValue})
	f = secretFixture(t, store, func(o *Options) {
		o.Git = g
		o.AskpassScript = "/state/bin/git-askpass"
	})
	spec := withOrigin(t, secretSpec("echo hi", "headless", map[string]string{"GH": harnessRef}), originURL)
	k = f.assign(spec, func(a *v1.AssignCommand) { a.Repo = originURL })
	if f.e.decideOnce(t.Context()) || f.run(k).Phase != state.PhaseAccepted {
		t.Fatalf("not accepted: %s", f.run(k).Phase)
	}
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	dir := f.runDir(k)

	if atFetch.origin != nil || !errors.Is(atFetch.harness, secretjit.ErrNoAccess) {
		t.Errorf("at the fetch askpass reads the origin: %v; the harness ref: %v", atFetch.origin, atFetch.harness)
	}
	if got, err := secretjit.Resolver(dir)(harnessRef); err != nil || got != ghValue {
		t.Errorf("the harness reads its own secret: %q, %v", got, err)
	}
	if _, err := secretjit.Resolver(dir)(originRef); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("the harness reads the origin credential: %v", err)
	}
	for _, ref := range []string{originRef, harnessRef} {
		if _, err := secretjit.AskpassReader(dir)(ref); !errors.Is(err, secretjit.ErrNoAccess) {
			t.Errorf("askpass reads %s after the fetch: %v", ref, err)
		}
	}
	needles, err := secretjit.Load(dir)
	if err != nil || len(needles) != 2 {
		t.Fatalf("retention = %v, %v", needles, err)
	}
	for _, n := range needles {
		wantUser := ""
		if n.Ref == originRef {
			wantUser = secrets.DefaultUser
		}
		if n.User != wantUser {
			t.Errorf("%s retained with user %q, want %q", n.Ref, n.User, wantUser)
		}
	}
	var l execwrap.Launch
	if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); !ok || err != nil || !l.RedactOutput {
		t.Errorf("launch = %+v, %v", l, err)
	}
}

func TestScrubKeepsTheIdentityOfTheErrorItRedacts(t *testing.T) {
	f, k := originRun(t, 2, &fakeGit{})
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	cut := fmt.Errorf("%w: the prepare was cut short: %w", errPrepareLater,
		&codedFailure{code: domain.CodeSecretUnavailable, msg: "reading " + originValue})
	got := f.e.scrub(f.run(k), cut)
	if !errors.Is(got, errPrepareLater) {
		t.Errorf("a prepare that was cut short is no longer retryable: %v", got)
	}
	var coded *codedFailure
	if !errors.As(got, &coded) || coded.code != domain.CodeSecretUnavailable {
		t.Fatalf("the code is lost: %v", got)
	}
	if strings.Contains(got.Error(), originValue) || strings.Contains(coded.msg, originValue) {
		t.Errorf("the value is in %q / %q", got, coded.msg)
	}
}

// An origin-only Run is a Run with secrets all the same: its harness cannot
// publish the credential of the origin, and the needles are gone with the
// release.
func TestAnOriginOnlyRunCannotPublishItsCredentialAndTheNeedlesGoWithTheRelease(t *testing.T) {
	g := &fakeGit{head: headOID, guard: blockOnValues}
	store := newFakeStore(map[string]string{originRef: originValue})
	f := secretFixture(t, store, func(o *Options) {
		o.Git = g
		o.AskpassScript = "/state/bin/git-askpass"
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	})
	k := f.assign(originSpec(4, originURL), func(a *v1.AssignCommand) { a.Repo = originURL })
	if f.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	if err := f.e.prepare(t.Context(), f.run(k)); !errors.Is(err, errAckPending) {
		t.Fatalf("first prepare = %v, want the ack wait", err)
	}
	f.confirmReports(k)
	f.e.step(t.Context(), k)
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running; reports %v", got, kinds(f.reports(k)))
	}
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: exitedAt, Cause: execwrap.CauseExit})
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run was not settled")
	}
	f.turn()

	if p := f.publication(k); p.State != state.PubFailed || g.pushCount() != 0 {
		t.Fatalf("publication = %s after %d pushes", p.State, g.pushCount())
	}
	g.mu.Lock()
	checked := append([]gitops.Forbidden(nil), g.forbidden...)
	g.mu.Unlock()
	if len(checked) == 0 || len(checked[0].Values) != 1 || string(checked[0].Values[0].Value) != originValue {
		t.Fatalf("the sealing was not checked against the credential of the origin: %+v", checked)
	}
	f.releaseTick()
	if f.delivered(k) {
		t.Error("the needles are left after the release")
	}
	if _, err := secretjit.Load(f.runDir(k)); !errors.Is(err, secretjit.ErrNotDelivered) {
		t.Errorf("Load after the release = %v", err)
	}
}

// After the token and the user rotate between attempts, what the publication
// is checked against holds both versions, so the guard blocks either.
func TestThePublicationGuardCoversBothVersionsOfARotatedOriginCredential(t *testing.T) {
	store := newFakeStore(map[string]string{originRef: originValue})
	f := secretFixture(t, store)
	k := f.assign(originSpec(4, originURL), func(a *v1.AssignCommand) { a.Repo = originURL })
	dir := f.runDir(k)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := secretjit.New()
	allow := func() error { return nil }
	versions := []struct{ token, user string }{{"first-origin-token-aaaa", "x-access-token"}, {"second-origin-token-bbb", "deploy-bot"}}
	for i, v := range versions {
		if err := m.DeliverFor(dir, uint64(i+1), []secretjit.Consumer{secretjit.Askpass}, []redact.Needle{{Ref: originRef, Value: []byte(v.token)}}, allow); err != nil {
			t.Fatal(err)
		}
		if err := m.Bind(dir, originRef, secretjit.Binding{Origin: originURL, User: v.user}); err != nil {
			t.Fatal(err)
		}
	}
	forbidden, err := f.e.forbidden(f.run(k))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		basic := base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(v.user + ":" + v.token))
		for _, leak := range []string{v.token, basic} {
			if got := forbidden.Redact("remote: " + leak); strings.Contains(got, leak) {
				t.Errorf("%q passes the guard of the publication", leak)
			}
		}
	}
}
