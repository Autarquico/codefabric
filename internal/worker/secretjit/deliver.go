package secretjit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// Deliver puts secrets in the run directory runDir as attempt, the prepare
// attempt that was granted them, and lets the harness read all of them.
//
// verify is the caller's fence: Deliver calls it under the lock before it
// changes anything and again just before it publishes the delivery, and stops
// with its error if it fails. The caller makes it fail once the Run can no
// longer receive secrets (it stopped, or another attempt took over); since
// Release holds the same lock, a delivery cannot come back after a release
// that the caller fenced. verify must not wait on this package.
//
// It is idempotent: a delivery of the same attempt with the same values is
// left as it is, including what was revoked; an older one, or one that is
// corrupt or unsafe, is replaced; a newer one is ErrStale. A crash at any
// point leaves secrets/ absent, or complete with the old or the new values,
// and calling Deliver or Release again converges.
//
// secrets must be 1 to domain.MaxRunSecrets distinct, valid refs, each with a
// value redact.NewSet accepts: a delivery is never something the output
// cannot be redacted of.
func (m *Manager) Deliver(runDir string, attempt uint64, secrets []redact.Needle, verify func() error) error {
	if err := checkSecrets(attempt, secrets); err != nil {
		return err
	}
	unlock, err := lock(runDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := verify(); err != nil {
		return err
	}
	if d, err := openDelivery(runDir); err == nil {
		current := d.m.Attempt
		same := current == attempt && d.holds(secrets)
		d.close()
		switch {
		case current > attempt:
			return fmt.Errorf("%w: attempt %d, and %d is in place", ErrStale, attempt, current)
		case same:
			return m.tidy(runDir)
		}
	}
	return m.publish(runDir, attempt, secrets, verify)
}

func checkSecrets(attempt uint64, secrets []redact.Needle) error {
	if attempt == 0 || len(secrets) == 0 {
		return fmt.Errorf("secretjit: nothing to deliver: attempt %d with %d secrets", attempt, len(secrets))
	}
	refs := make([]string, len(secrets))
	for i, s := range secrets {
		refs[i] = s.Ref
		if err := domain.CheckSecretValue(s.Value); err != nil {
			return fmt.Errorf("secret %q: %w", s.Ref, err)
		}
	}
	if err := domain.CheckSecretRefs(refs, domain.MaxRunSecrets); err != nil {
		return fmt.Errorf("secretjit: %w", err)
	}
	if _, err := redact.NewSet(secrets); err != nil {
		return fmt.Errorf("secretjit: the output could not be redacted: %w", err)
	}
	return nil
}

// holds reports whether the delivery has exactly secrets, value for value.
func (d *delivery) holds(secrets []redact.Needle) bool {
	if len(d.m.Refs) != len(secrets) {
		return false
	}
	for _, s := range secrets {
		e, ok := d.find(s.Ref)
		if !ok {
			return false
		}
		v, err := d.value(e)
		if err != nil || !bytes.Equal(v, s.Value) {
			return false
		}
	}
	return true
}

// publish builds the delivery in staging and renames it into place.
func (m *Manager) publish(runDir string, attempt uint64, secrets []redact.Needle, verify func() error) error {
	staging := filepath.Join(runDir, stagingName)
	if err := m.remove(staging); err != nil {
		return err
	}
	if err := m.fs.mkdir(staging); err != nil {
		return m.abandon(staging, fmt.Errorf("secretjit: creating %s: %w", staging, err))
	}
	man := manifest{Schema: manifestSchema, Attempt: attempt}
	for _, s := range secrets {
		e := entry{Ref: s.Ref, File: fileName(s.Ref), Harness: true}
		if err := m.writeSynced(filepath.Join(staging, e.File), s.Value); err != nil {
			return m.abandon(staging, fmt.Errorf("secretjit: writing secret %q: %w", s.Ref, err))
		}
		man.Refs = append(man.Refs, e)
	}
	data, err := json.Marshal(man)
	if err != nil {
		return m.abandon(staging, fmt.Errorf("secretjit: %w", err))
	}
	if err := m.writeSynced(filepath.Join(staging, manifestName), data); err != nil {
		return m.abandon(staging, fmt.Errorf("secretjit: writing %s: %w", manifestName, err))
	}
	if err := m.fs.sync(staging); err != nil {
		return m.abandon(staging, fmt.Errorf("secretjit: syncing %s: %w", staging, err))
	}
	if err := verify(); err != nil {
		return m.abandon(staging, err)
	}
	live, deleting := filepath.Join(runDir, liveName), filepath.Join(runDir, deletingName)
	if err := m.retire(runDir, live, deleting); err != nil {
		return m.abandon(staging, err)
	}
	if err := m.fs.rename(staging, live); err != nil {
		return m.abandon(staging, fmt.Errorf("secretjit: publishing %s: %w", live, err))
	}
	if err := m.fs.sync(runDir); err != nil {
		return fmt.Errorf("secretjit: syncing %s: %w", runDir, err)
	}
	return m.tidy(runDir)
}

// writeSynced writes data to path, which must not exist, and syncs it.
func (m *Manager) writeSynced(path string, data []byte) error {
	if err := m.fs.writeFile(path, data); err != nil {
		return err
	}
	return m.fs.sync(path)
}

// abandon removes a staging directory, as well as it can, and returns err.
// A crash skips it: the next Deliver or Release removes staging.
func (m *Manager) abandon(staging string, err error) error {
	_ = m.fs.removeAll(staging)
	return err
}

// retire moves the delivery in live, if any, to deleting, where it is no
// longer visible but still has to be removed.
func (m *Manager) retire(runDir, live, deleting string) error {
	ok, err := present(live)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPending, err)
	}
	if !ok {
		return nil
	}
	if err := m.remove(deleting); err != nil {
		return err
	}
	if err := m.fs.rename(live, deleting); err != nil {
		return fmt.Errorf("%w: retiring %s: %w", ErrPending, live, err)
	}
	if err := m.fs.sync(runDir); err != nil {
		return fmt.Errorf("%w: syncing %s: %w", ErrPending, runDir, err)
	}
	return nil
}

// tidy removes what a delivery leaves behind besides secrets/: staging and
// a retired delivery.
func (m *Manager) tidy(runDir string) error {
	return errors.Join(
		m.remove(filepath.Join(runDir, stagingName)),
		m.remove(filepath.Join(runDir, deletingName)),
	)
}

// remove removes path and everything under it; a failure is ErrPending.
func (m *Manager) remove(path string) error {
	if err := m.fs.removeAll(path); err != nil {
		return fmt.Errorf("%w: removing %s: %w", ErrPending, path, err)
	}
	return nil
}
