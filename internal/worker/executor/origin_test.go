//go:build linux || darwin

package executor

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
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
