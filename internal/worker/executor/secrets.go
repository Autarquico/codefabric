package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The secrets of a Run (§16.7). The Worker checks them twice on its own, when
// it evaluates the assignment and again before it reads a value, but only the
// Controller decides: every prepare attempt asks it for a grant first, and a
// value is read only once it is given. The values go to the run directory
// (secretjit), where the wrapper resolves them just before the harness starts
// and the capture redacts them from the output; launch.json, state.db and the
// reports carry refs only.

var (
	// errPrepareLater is a prepare that could not finish now and may later:
	// the Run stays accepted and the prepare is tried again, as a new attempt.
	errPrepareLater = errors.New("the prepare is tried again later")
	// errPrepareFenced is a grant the Controller refused because the lease is
	// no longer this Worker's to prepare: the Run is fenced, as by a stale
	// report, and waits for the reconciliation to say what it is.
	errPrepareFenced = errors.New("the lease is no longer this Worker's to prepare")
)

// Bounds of the retry of a prepare that could not finish.
const (
	prepareRetryInitial = 2 * time.Second
	prepareRetryMax     = time.Minute
)

// checkSpecSecrets is an error unless the secrets of a spec are well formed:
// schema 3 exactly when it has any, valid refs and names, and an environment
// whose refs are among those it declares.
func checkSpecSecrets(spec specView) error {
	refs := spec.refs()
	switch {
	case spec.Schema == domain.RunSpecSchemaSecrets && len(refs) == 0:
		return errors.New("the spec has schema 3 and declares no secret")
	case spec.Schema != domain.RunSpecSchemaSecrets && spec.Secrets != nil:
		return fmt.Errorf("the spec has schema %d and declares secrets", spec.Schema)
	}
	if err := domain.CheckSecretRefs(refs, domain.MaxRunSecrets); err != nil {
		return err
	}
	env := spec.env()
	if err := domain.CheckSecretEnv(env); err != nil {
		return err
	}
	for _, e := range env {
		if !slices.Contains(refs, e.Ref) {
			return fmt.Errorf("variable %q names ref %q, which the spec does not declare", e.Name, e.Ref)
		}
	}
	return nil
}

// secretsVerdict is the Worker's own check of the secrets of an assignment:
// it holds every ref, of a backend it can read, and its scope lets the
// Project of the Run use it. The Controller checked the same before it
// assigned the Run, from what the heartbeat said; the catalog may have
// changed since.
func (e *Executor) secretsVerdict(ctx context.Context, r state.Run, spec specView) verdict {
	if err := checkSpecSecrets(spec); err != nil {
		return verdict{domain.CodeVersionIncompatible, "the secrets of the spec cannot be read: " + err.Error()}
	}
	refs := spec.refs()
	if len(refs) == 0 {
		return verdict{}
	}
	if code, msg := e.localPolicy(ctx, r.ProjectID, refs); code != "" {
		return verdict{code, msg}
	}
	return verdict{}
}

// localPolicy checks refs against the catalog of this Worker for a Run of
// project: CF_SECRET_UNAVAILABLE for a ref it does not hold or cannot read,
// CF_POLICY_DENIED for one it holds only for other Projects.
func (e *Executor) localPolicy(ctx context.Context, project string, refs []string) (domain.Code, string) {
	if e.o.Secrets == nil {
		return domain.CodeSecretUnavailable, "this Worker keeps no secrets: " + refList(refs)
	}
	catalog, err := e.o.Secrets.List(ctx)
	if err != nil {
		e.log.WarnContext(ctx, "the catalog of secrets cannot be read", "err", err)
		return domain.CodeSecretUnavailable, "the catalog of secrets of this Worker cannot be read"
	}
	return scopeVerdict(project, refs, catalog)
}

// scopeVerdict checks refs against catalog for a Run of project.
func scopeVerdict(project string, refs []string, catalog []secrets.Metadata) (domain.Code, string) {
	byRef := make(map[string]secrets.Metadata, len(catalog))
	for _, m := range catalog {
		byRef[m.Ref] = m
	}
	var missing, outOfScope []string
	for _, ref := range refs {
		m, ok := byRef[ref]
		switch {
		case !ok || m.Backend != secrets.BackendFile:
			missing = append(missing, ref)
		case len(m.Projects) > 0 && !slices.Contains(m.Projects, project):
			outOfScope = append(outOfScope, ref)
		}
	}
	switch {
	case len(missing) > 0:
		return domain.CodeSecretUnavailable, "this Worker does not hold the secrets " + refList(missing)
	case len(outOfScope) > 0:
		return domain.CodePolicyDenied, "this Worker holds the secrets " + refList(outOfScope) + " only for other Projects"
	}
	return "", ""
}

// refList names refs for a message, a few of them when they are many.
func refList(refs []string) string {
	const shown = 5
	quoted := make([]string, 0, min(len(refs), shown))
	for _, ref := range refs[:min(len(refs), shown)] {
		quoted = append(quoted, fmt.Sprintf("%q", ref))
	}
	list := strings.Join(quoted, ", ")
	if len(refs) > shown {
		list += fmt.Sprintf(" and %d more", len(refs)-shown)
	}
	return list
}

// usesSecrets says whether the Run r has secrets on this Worker: the spec
// declares them, or its configuration was rendered with some (the Controller
// does not know the refs of the configuration yet, so the spec cannot say so).
func (e *Executor) usesSecrets(r state.Run) bool {
	return len(parseSpec(r.Spec).refs()) > 0 || secretjit.HasRendered(e.runDir(r.Key))
}

// snapshotRefs are the refs of the markers of the files of snap, sorted and
// without repeats. A file whose refs are not those of its content is refused,
// wrapping harness.ErrSecretUnavailable.
func snapshotRefs(snap harness.NativeSnapshot) ([]string, error) {
	var refs []string
	for _, c := range snap.Components {
		for _, f := range c.Files {
			if err := f.CheckSecrets(); err != nil {
				return nil, fmt.Errorf("component %q: %w", c.Name, err)
			}
			refs = append(refs, f.SecretRefs...)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

// configOf is the configuration the Run r is materialised from, and the refs
// of the secrets its files carry.
func (e *Executor) configOf(ctx context.Context, r state.Run, adapter harness.Adapter) (harness.NativeSnapshot, []string, error) {
	snap, err := e.o.ConfigSource(ctx, r, adapter.ID())
	if err != nil {
		return harness.NativeSnapshot{}, nil, fmt.Errorf("the configuration of the harness %q: %w", adapter.ID(), err)
	}
	refs, err := snapshotRefs(snap)
	if err != nil {
		return harness.NativeSnapshot{}, nil, fmt.Errorf("%s: the configuration of the harness %q: %w", domain.CodeSecretUnavailable, adapter.ID(), err)
	}
	return snap, refs, nil
}

// configSecretsVerdict is secretsVerdict for the secrets of the configuration
// of the harness. The Controller cannot know them yet (the snapshot is the
// Worker's), so this is where the Run is turned away before it is accepted,
// for a secret this Worker cannot give it; the Controller still decides every
// grant. A configuration that cannot be read now is the prepare's to report.
func (e *Executor) configSecretsVerdict(ctx context.Context, r state.Run, adapter harness.Adapter) verdict {
	if !adapter.Capabilities().ConfigIsolation {
		return verdict{}
	}
	_, refs, err := e.configOf(ctx, r, adapter)
	switch {
	case errors.Is(err, harness.ErrSecretUnavailable):
		return verdict{domain.CodeSecretUnavailable, err.Error()}
	case err != nil || len(refs) == 0:
		return verdict{}
	}
	if code, msg := e.localPolicy(ctx, r.ProjectID, refs); code != "" {
		return verdict{code, msg}
	}
	return verdict{}
}

// secretPrepare is one prepare attempt of a Run with secrets: its number,
// taken in state.db before anything is asked, and the refs granted to it.
type secretPrepare struct {
	e       *Executor
	r       state.Run
	attempt uint64
	granted []string
}

// beginSecrets takes the next prepare attempt of r. It fails if r is no
// longer accepted.
func (e *Executor) beginSecrets(ctx context.Context, r state.Run) (*secretPrepare, error) {
	var attempt uint64
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		var err error
		attempt, err = state.NextPrepareAttempt(ctx, tx, r.Key)
		return err
	})
	if errors.Is(err, state.ErrChanged) {
		return nil, fmt.Errorf("%w: the Run is no longer accepted", errPrepareLater)
	}
	if err != nil {
		return nil, err
	}
	return &secretPrepare{e: e, r: r, attempt: attempt}, nil
}

// grant asks the Controller for refs, in this attempt.
func (p *secretPrepare) grant(ctx context.Context, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	if !p.e.heartbeatTaken.Load() {
		// The Controller would refuse it as a daemon it has retired, and fence the Run.
		return fmt.Errorf("%w: the Controller has not taken a heartbeat of this daemon yet", errPrepareLater)
	}
	if err := p.current(ctx); err != nil {
		return err
	}
	resp, err := p.e.o.Controller.GrantSecrets(ctx, p.r.RunID, v1.GrantSecretsRequest{
		LeaseID: p.r.LeaseID, LeaseEpoch: p.r.LeaseEpoch, WorkerEpoch: p.e.o.Journal.Epoch(), Refs: refs,
	})
	if err != nil {
		return grantError(err)
	}
	for _, ref := range refs {
		if !slices.Contains(resp.Granted, ref) {
			return fmt.Errorf("%s: the Controller answered the grant without the secret %q", domain.CodeSecretUnavailable, ref)
		}
	}
	p.granted = append(p.granted, refs...)
	return nil
}

// grantError is what a refused grant means for the prepare. A lease that is
// not this Worker's to prepare fences the Run; a Controller that cannot be
// reached, or does not know the caller now, is asked again later; a denial is
// the failure of the prepare, with the code of the grant and only refs.
func grantError(err error) error {
	var coded *domain.Error
	if !errors.As(err, &coded) {
		return fmt.Errorf("%w: asking for the secrets: %w", errPrepareLater, err)
	}
	switch coded.Code {
	case domain.CodeStaleLease, domain.CodeNotFound:
		return fmt.Errorf("%w: %s: %s", errPrepareFenced, coded.Code, coded.Message)
	case domain.CodeControllerUnreachable, domain.CodeInternal, domain.CodeUnauthenticated:
		return fmt.Errorf("%w: asking for the secrets: %s: %s", errPrepareLater, coded.Code, coded.Message)
	case domain.CodePolicyDenied:
		return fmt.Errorf("%s: the Controller denied the secrets %s", coded.Code, denials(coded.Details))
	}
	return fmt.Errorf("%s: asking for the secrets: %s", coded.Code, coded.Message)
}

// denials names the refs of details.denied and why each was denied. The
// details come decoded from JSON, or as they were made in-process: they are
// read through JSON either way.
func denials(details map[string]any) string {
	var denied []v1.SecretDenial
	if raw, err := json.Marshal(details["denied"]); err == nil {
		_ = json.Unmarshal(raw, &denied)
	}
	if len(denied) == 0 {
		return "(the Controller did not say which)"
	}
	parts := make([]string, 0, len(denied))
	for _, d := range denied {
		parts = append(parts, fmt.Sprintf("%q (%s)", d.Ref, d.Reason))
	}
	return strings.Join(parts, ", ")
}

// current is an error unless the Run is still accepted, unfenced, and this is
// its latest prepare attempt: what is checked before every effect.
func (p *secretPrepare) current(ctx context.Context) error {
	r, err := state.RunBy(ctx, p.e.o.DB.Reader(), p.r.Key)
	switch {
	case err != nil:
		return fmt.Errorf("%w: reading the Run: %w", errPrepareLater, err)
	case r.Phase != state.PhaseAccepted || r.FencedCode != "" || r.PrepareAttempt != p.attempt:
		return fmt.Errorf("%w: the Run moved on from prepare attempt %d", errPrepareLater, p.attempt)
	}
	return nil
}

// deliver reads the values of the granted refs and puts them in the run
// directory, fenced by the phase and the attempt. Nothing is delivered that
// the output could not be redacted of: values too short, or too many, stop
// the Run before it starts.
func (p *secretPrepare) deliver(ctx context.Context, dir string) (map[string][]byte, error) {
	refs := slices.Sorted(slices.Values(p.granted))
	if err := domain.CheckSecretRefs(refs, domain.MaxRunSecrets); err != nil {
		return nil, fmt.Errorf("%s: %w", domain.CodeSecretUnavailable, err)
	}
	if p.e.o.Secrets == nil {
		return nil, fmt.Errorf("%s: this Worker keeps no secrets: %s", domain.CodeSecretUnavailable, refList(refs))
	}
	// The catalog may have changed since the assignment was evaluated: the
	// scope checked is that of each value read, never of another version.
	held := make([]secrets.Metadata, 0, len(refs))
	needles := make([]redact.Needle, 0, len(refs))
	for _, ref := range refs {
		s, err := p.e.o.Secrets.Get(ctx, ref)
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, fmt.Errorf("%s: this Worker no longer holds the secret %q", domain.CodeSecretUnavailable, ref)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: the secret %q cannot be read: %w", domain.CodeSecretUnavailable, ref, err)
		}
		held = append(held, s.Metadata)
		needles = append(needles, redact.Needle{Ref: ref, Value: s.Value})
	}
	if code, msg := scopeVerdict(p.r.ProjectID, refs, held); code != "" {
		return nil, fmt.Errorf("%s: %s", code, msg)
	}
	if _, err := redact.NewSet(needles); err != nil {
		return nil, fmt.Errorf("%s: the output could not be redacted of the secrets: %w", domain.CodeSecretUnavailable, err)
	}
	err := p.e.jit.Deliver(dir, p.attempt, needles, func() error { return p.current(ctx) })
	switch {
	case err == nil:
		values := make(map[string][]byte, len(needles))
		for _, n := range needles {
			values[n.Ref] = n.Value
		}
		return values, nil
	case errors.Is(err, errPrepareLater):
		return nil, err
	case errors.Is(err, secretjit.ErrStale):
		return nil, fmt.Errorf("%w: %w", errPrepareLater, err)
	}
	return nil, fmt.Errorf("%s: delivering the secrets: %w", domain.CodeSecretUnavailable, err)
}

// render writes the values into the files of the configuration that carry
// their markers, fenced like the delivery and registered before any value is
// written (secretjit.Manager.Render).
func (p *secretPrepare) render(ctx context.Context, dir string, mat host.Materialization, values map[string][]byte) error {
	fence := func(paths []string, write func() error) error {
		return p.e.jit.Render(dir, p.attempt, paths, func() error { return p.current(ctx) }, write)
	}
	err := p.e.materializer.Render(dir, mat, values, fence)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errPrepareLater):
		return err
	case errors.Is(err, secretjit.ErrStale):
		return fmt.Errorf("%w: %w", errPrepareLater, err)
	}
	return fmt.Errorf("%s: rendering the secrets of the configuration: %w", domain.CodeSecretUnavailable, err)
}

// launchSecretEnv is the secret environment of the harness: what the spec
// declares and what the adapter asks for. A name in both is refused. extra
// are the refs the adapter asks for that the spec does not declare: they go
// to the Controller like any other, which is where a ref is denied.
func launchSecretEnv(spec specView, adapter []harness.SecretEnv) (env []execwrap.SecretEnv, extra []string, err error) {
	declared := spec.refs()
	names := map[string]bool{}
	add := func(name, ref string) error {
		if names[name] {
			return fmt.Errorf("%s: the secret variable %q is set twice", domain.CodeInvalidRequest, name)
		}
		names[name] = true
		env = append(env, execwrap.SecretEnv{Name: name, Ref: ref})
		return nil
	}
	for _, s := range spec.env() {
		if err := add(s.Name, s.Ref); err != nil {
			return nil, nil, err
		}
	}
	for _, s := range adapter {
		if err := add(s.Name, s.Ref); err != nil {
			return nil, nil, err
		}
		if !slices.Contains(declared, s.Ref) && !slices.Contains(extra, s.Ref) {
			extra = append(extra, s.Ref)
		}
	}
	slices.Sort(extra)
	return env, extra, nil
}

// revokeOnce takes from the harness of a Run with secrets its access to them,
// once it has started: the wrapper resolved them before it did, and nothing
// else is to read them. What the output is redacted of stays until the
// release. It is done once per life of the daemon; doing it again is harmless.
func (e *Executor) revokeOnce(ctx context.Context, r state.Run) {
	if !e.usesSecrets(r) {
		return
	}
	e.mu.Lock()
	done := e.memo(r.Key).revoked
	e.mu.Unlock()
	if done {
		return
	}
	if err := e.jit.Revoke(e.runDir(r.Key), nil); err != nil {
		e.log.WarnContext(ctx, "taking the secrets back from a harness that started failed; it is tried again", "run", r.RunID, "err", err)
		return
	}
	e.mu.Lock()
	e.memo(r.Key).revoked = true
	e.mu.Unlock()
}
