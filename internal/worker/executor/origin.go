package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The credential of the origin (§16.7). The Run froze the ref and the URL it
// is for; this Worker fetches that URL, and only it, with git asking the
// askpass script for the value. The value is delivered to the run directory
// for askpass alone, never for the harness, and taken back from askpass as
// soon as the last fetch is over; the delivery stays (secretjit.Load) so the
// output is redacted of it and the publication is checked against it.

// specOrigin is the frozen origin of a Run: the ref of its credential and the
// URL of the repository it authenticates.
type specOrigin struct {
	Secret string `json:"secret"`
	URL    string `json:"url"`
}

// checkSpecOrigin is an error unless the origin of spec is well formed and is
// the repository repo the assignment names. A Run without an origin passes.
func checkSpecOrigin(spec specView, repo string) (domain.Code, error) {
	o := spec.Origin
	switch {
	case o == nil:
		return "", nil
	case domain.CheckSecretRefs([]string{o.Secret}, 1) != nil:
		return domain.CodeVersionIncompatible, errors.New("the origin of the spec names an invalid secret ref")
	case !domain.ValidRepoURL(o.URL) || !strings.HasPrefix(o.URL, "https://"):
		return domain.CodeVersionIncompatible, errors.New("the origin of the spec is not an https repository URL")
	case slices.Contains(spec.refs(), o.Secret):
		return domain.CodeVersionIncompatible, errors.New("the origin secret is also declared for the harness, which never receives it")
	case repo != o.URL:
		return domain.CodePolicyDenied, errors.New("the repository of the assignment is not the origin the Run froze")
	}
	return "", nil
}

// originAccess is the credential of the origin of one prepare attempt: it is
// granted and delivered on first use, and taken back by revoke. A nil
// *originAccess is a Run without one.
type originAccess struct {
	e       *Executor
	r       state.Run
	origin  specOrigin
	attempt func() (*secretPrepare, error)

	p         *secretPrepare
	delivered bool
	auth      *gitops.OriginAuth
	closed    bool
}

// originOf is the origin credential of the Run r, or nil if it has none or
// this Worker has no git to fetch it with.
func (e *Executor) originOf(r state.Run, spec specView, attempt func() (*secretPrepare, error)) *originAccess {
	if spec.Origin == nil || e.o.Git == nil {
		return nil
	}
	return &originAccess{e: e, r: r, origin: *spec.Origin, attempt: attempt}
}

// credential is what the fetches of origin run with. The first call does, in
// this order: the run directory, the Controller's grant, the read of the value
// and its user, the delivery for askpass alone, and the binding to the origin
// URL and the user. After revoke it is an error: access is not given back
// within an attempt.
func (o *originAccess) credential(ctx context.Context) (*gitops.OriginAuth, error) {
	switch {
	case o == nil:
		return nil, nil
	case o.auth != nil:
		return o.auth, nil
	case o.closed:
		return nil, errors.New("the credential of the origin was already taken back in this attempt")
	case o.e.o.AskpassScript == "":
		return nil, errors.New("this Worker has no askpass script: the origin of the Run cannot be fetched")
	}
	e, ref := o.e, o.origin.Secret
	p, err := o.attempt()
	if err != nil {
		return nil, err
	}
	o.p = p
	dir := e.runDir(o.r.Key)
	if err := mkdir(dir); err != nil {
		return nil, err
	}
	if err := p.request(ctx, []string{ref}); err != nil {
		return nil, err
	}
	if e.o.Secrets == nil {
		return nil, &codedFailure{domain.CodeSecretUnavailable, "this Worker keeps no secrets: " + refList([]string{ref})}
	}
	s, err := e.o.Secrets.Get(ctx, ref)
	if err != nil {
		return nil, readFailure(ctx, ref, err)
	}
	if code, msg := scopeVerdict(o.r.ProjectID, []string{ref}, []secrets.Metadata{s.Metadata}); code != "" {
		return nil, fmt.Errorf("%s: %s", code, msg)
	}
	needles := []redact.Needle{{Ref: ref, Value: s.Value}}
	if _, err := redact.NewEscapedSet(needles); err != nil {
		return nil, fmt.Errorf("%s: the output could not be redacted of the secrets: %w", domain.CodeSecretUnavailable, err)
	}
	user := s.User
	if user == "" {
		user = secrets.DefaultUser
	}
	o.delivered = true
	if err := deliveryError(e.jit.DeliverFor(dir, p.attempt, []secretjit.Consumer{secretjit.Askpass}, needles, func() error { return p.current(ctx) })); err != nil {
		return nil, err
	}
	if err := e.jit.Bind(dir, ref, secretjit.Binding{Origin: o.origin.URL, User: user}); err != nil {
		return nil, fmt.Errorf("%s: binding the credential of the origin: %w", domain.CodeSecretUnavailable, err)
	}
	retained, err := secretjit.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: reading back the credential of the origin: %w", domain.CodeSecretUnavailable, err)
	}
	o.auth = &gitops.OriginAuth{Askpass: e.o.AskpassScript, RunDir: dir, Needles: retained}
	return o.auth, nil
}

// revoke takes the credential back from askpass, fenced by the attempt that
// delivered it: a delivery of a newer attempt is not touched. It is safe to
// call when nothing was delivered, and again. The delivery itself stays.
func (o *originAccess) revoke() error {
	if o == nil {
		return nil
	}
	o.auth, o.closed = nil, true
	if !o.delivered {
		return nil
	}
	err := o.e.jit.RevokeAttempt(o.e.runDir(o.r.Key), o.p.attempt, secretjit.Askpass, []string{o.origin.Secret})
	if err == nil || errors.Is(err, secretjit.ErrStale) {
		o.delivered = false
		return nil
	}
	return err
}

// deliveryError is what a failed secretjit delivery means for the prepare.
func deliveryError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errPrepareLater):
		return err
	case errors.Is(err, secretjit.ErrStale):
		return fmt.Errorf("%w: %w", errPrepareLater, err)
	}
	return fmt.Errorf("%s: delivering the secrets: %w", domain.CodeSecretUnavailable, err)
}

// scrubbed is an error whose text has been redacted of the secrets of the
// Run. It keeps the identity of the original: errors.Is answers as the
// original does (errPrepareLater and the like still match), and errors.As
// finds the redacted coded failure if the chain has one, and the original
// chain otherwise.
type scrubbed struct {
	msg    string
	err    error // the original
	unwrap error // what errors.As walks: err, with a coded failure redacted
}

func (s *scrubbed) Error() string        { return s.msg }
func (s *scrubbed) Unwrap() error        { return s.unwrap }
func (s *scrubbed) Is(target error) bool { return errors.Is(s.err, target) }

// scrub redacts err of what the Run retains, so that neither the report of a
// failed prepare nor the log carries a value. A Run whose retention cannot be
// read says so and nothing of err's text.
func (e *Executor) scrub(r state.Run, err error) error {
	if err == nil || !e.usesSecrets(r) {
		return err
	}
	needles, loadErr := secretjit.Load(e.runDir(r.Key))
	if errors.Is(loadErr, secretjit.ErrNotDelivered) {
		return err
	}
	redactText := func(s string) string {
		if loadErr != nil {
			return "(the message is withheld: the secrets of the Run cannot be read to redact it)"
		}
		set, setErr := redact.NewEscapedSet(needles)
		if setErr != nil {
			return "(the message is withheld: the secrets of the Run cannot be redacted)"
		}
		return string(redact.Bytes([]byte(s), set))
	}
	unwrap := err
	var coded *codedFailure
	if errors.As(err, &coded) {
		unwrap = &codedFailure{code: coded.code, msg: redactText(coded.msg)}
	}
	return &scrubbed{msg: redactText(err.Error()), err: err, unwrap: unwrap}
}
