package secretjit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// item is a secret on its way into a delivery, with who may read it.
type item struct {
	ref    string
	value  []byte
	access []Consumer
}

func needlesOf(items []item) []redact.Needle {
	out := make([]redact.Needle, len(items))
	for i, it := range items {
		out[i] = redact.Needle{Ref: it.ref, Value: it.value}
	}
	return out
}

// Deliver puts secrets in the run directory runDir as attempt, the prepare
// attempt that was granted them, and lets the harness read all of them. It is
// DeliverFor with the harness as the only consumer.
func (m *Manager) Deliver(runDir string, attempt uint64, secrets []redact.Needle, verify func() error) error {
	return m.DeliverFor(runDir, attempt, []Consumer{Harness}, secrets, verify)
}

// DeliverFor puts secrets in the run directory runDir as attempt, the prepare
// attempt that was granted them, and lets the consumers in access read them.
//
// verify is the caller's fence: DeliverFor calls it under the lock before it
// changes anything and again just before it publishes the delivery, and stops
// with its error if it fails. The caller makes it fail once the Run can no
// longer receive secrets (it stopped, or another attempt took over); since
// Release holds the same lock, a delivery cannot come back after a release
// that the caller fenced. verify must not wait on this package.
//
// A delivery of the attempt already in place is merged into it, never
// replaced: the refs already there keep their values, their access and what
// was revoked of it (a ref granted again is not widened, and is not repeated),
// and the new refs are added with access. A ref already there with another
// value is ErrConflict. A delivery that adds nothing is left as it is. An
// older attempt in place, or a delivery that is corrupt or unsafe, is
// replaced; a newer one is ErrStale. A crash at any point leaves secrets/
// absent, or complete with the old or the merged values, and calling
// DeliverFor or Release again converges.
//
// secrets must be 1 to domain.MaxRunSecrets distinct, valid refs, each with a
// value redact.NewSet accepts, and so must the merged delivery: a delivery is
// never something the output cannot be redacted of.
func (m *Manager) DeliverFor(runDir string, attempt uint64, access []Consumer, secrets []redact.Needle, verify func() error) error {
	access, err := checkSecrets(attempt, access, secrets)
	if err != nil {
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
	items := make([]item, len(secrets))
	for i, s := range secrets {
		items[i] = item{ref: s.Ref, value: s.Value, access: access}
	}
	if d, err := openDelivery(runDir); err == nil {
		current := d.m.Attempt
		if current > attempt {
			d.close()
			return fmt.Errorf("%w: attempt %d, and %d is in place", ErrStale, attempt, current)
		}
		if current == attempt {
			held := d.m
			merged, kept, err := d.merge(items)
			d.close()
			switch {
			case errors.Is(err, ErrConflict):
				return err
			case err == nil && len(merged) == kept:
				return m.tidy(runDir)
			case err == nil:
				if err := checkSet(needlesOf(merged)); err != nil {
					return err
				}
				return m.extend(runDir, held, merged[kept:], verify)
			}
		} else {
			d.close()
		}
	}
	return m.publish(runDir, attempt, items, verify)
}

// checkSecrets validates the arguments of a delivery and returns access
// sorted and without repeats.
func checkSecrets(attempt uint64, access []Consumer, secrets []redact.Needle) ([]Consumer, error) {
	if attempt == 0 || len(secrets) == 0 {
		return nil, fmt.Errorf("secretjit: nothing to deliver: attempt %d with %d secrets", attempt, len(secrets))
	}
	if len(access) == 0 || slices.ContainsFunc(access, func(c Consumer) bool { return !c.valid() }) {
		return nil, fmt.Errorf("secretjit: a delivery needs at least one known consumer, and has %d", len(access))
	}
	access = slices.Compact(slices.Sorted(slices.Values(access)))
	for _, s := range secrets {
		if err := domain.CheckSecretValue(s.Value); err != nil {
			return nil, fmt.Errorf("secret %q: %w", s.Ref, err)
		}
	}
	return access, checkSet(secrets)
}

// checkSet is an error unless secrets are valid, distinct refs that the
// output can be redacted of.
func checkSet(secrets []redact.Needle) error {
	refs := make([]string, len(secrets))
	for i, s := range secrets {
		refs[i] = s.Ref
	}
	if err := domain.CheckSecretRefs(refs, domain.MaxRunSecrets); err != nil {
		return fmt.Errorf("secretjit: %w", err)
	}
	if _, err := redact.NewEscapedSet(secrets); err != nil {
		return fmt.Errorf("secretjit: the output could not be redacted: %w", err)
	}
	return nil
}

// merge is the items of the delivery followed by those of items it does not
// hold, and how many of the first it holds. The refs it holds keep their value
// and their access. An item with the ref of one it holds but another value is
// ErrConflict.
func (d *delivery) merge(items []item) (merged []item, kept int, err error) {
	for _, e := range d.m.Refs {
		v, err := d.value(e)
		if err != nil {
			return nil, 0, err
		}
		merged = append(merged, item{ref: e.Ref, value: v, access: e.Access})
	}
	kept = len(merged)
	for _, it := range items {
		i := slices.IndexFunc(merged[:kept], func(h item) bool { return h.ref == it.ref })
		switch {
		case i < 0:
			merged = append(merged, it)
		case !bytes.Equal(merged[i].value, it.value):
			return nil, 0, fmt.Errorf("%w: %q", ErrConflict, it.ref)
		}
	}
	return merged, kept, nil
}

// extend adds added to the delivery in place, whose manifest is held. The
// delivery never goes away: the new value files are written first, where
// nothing names them, and the manifest that does is replaced atomically, so a
// crash leaves the delivery as it was or with all of added. A file a crash
// left unnamed is rewritten by the next try, or removed with the delivery.
func (m *Manager) extend(runDir string, held manifest, added []item, verify func() error) error {
	live := filepath.Join(runDir, liveName)
	man := manifest{Schema: held.Schema, Attempt: held.Attempt, Refs: slices.Clone(held.Refs)}
	for _, it := range added {
		e := entry{Ref: it.ref, File: fileName(it.ref), Access: it.access}
		path := filepath.Join(live, e.File)
		if err := m.remove(path); err != nil {
			return err
		}
		if err := m.writeSynced(path, it.value); err != nil {
			return fmt.Errorf("secretjit: writing secret %q: %w", it.ref, err)
		}
		man.Refs = append(man.Refs, e)
	}
	if err := m.fs.sync(live); err != nil {
		return fmt.Errorf("secretjit: syncing %s: %w", live, err)
	}
	if err := verify(); err != nil {
		return err
	}
	if err := m.replaceManifest(live, man); err != nil {
		return err
	}
	return m.tidy(runDir)
}

// publish builds the delivery in staging and renames it into place.
func (m *Manager) publish(runDir string, attempt uint64, items []item, verify func() error) error {
	staging := filepath.Join(runDir, stagingName)
	if err := m.remove(staging); err != nil {
		return err
	}
	if err := m.fs.mkdir(staging); err != nil {
		return m.abandon(staging, fmt.Errorf("secretjit: creating %s: %w", staging, err))
	}
	man := manifest{Schema: manifestSchema, Attempt: attempt}
	for _, it := range items {
		e := entry{Ref: it.ref, File: fileName(it.ref), Access: it.access}
		if err := m.writeSynced(filepath.Join(staging, e.File), it.value); err != nil {
			return m.abandon(staging, fmt.Errorf("secretjit: writing secret %q: %w", it.ref, err))
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
