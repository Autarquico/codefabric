package secretjit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Release removes the secrets of the run directory runDir: any staging or
// retired delivery and, if verify allows it, the delivery in place. verify is
// the caller's fence, called under the lock: it fails while the Run still
// needs its values (its capture or its publication are open), and Release
// then stops with its error, leaving the delivery whole.
//
// It reads nothing, so a delivery with a missing or corrupt manifest, or half
// built, is released all the same. It is idempotent and a crash at any point
// leaves either the whole delivery or none of it visible. It returns nil only
// when nothing is left; anything it could not remove is ErrPending, and
// calling it again finishes the job. A run directory that does not exist has
// nothing to release.
func (m *Manager) Release(runDir string, verify func() error) error {
	if ok, err := present(runDir); err != nil {
		return fmt.Errorf("%w: %w", ErrPending, err)
	} else if !ok {
		return nil
	}
	unlock, err := lock(runDir)
	if err != nil {
		return err
	}
	defer unlock()
	return m.release(runDir, verify)
}

func (m *Manager) release(runDir string, verify func() error) error {
	live, deleting := filepath.Join(runDir, liveName), filepath.Join(runDir, deletingName)
	if err := m.tidy(runDir); err != nil {
		return err
	}
	if ok, err := present(live); err != nil {
		return fmt.Errorf("%w: %w", ErrPending, err)
	} else if ok {
		if err := verify(); err != nil {
			return err
		}
		if err := m.retire(runDir, live, deleting); err != nil {
			return err
		}
		if err := m.remove(deleting); err != nil {
			return err
		}
	}
	if err := m.fs.sync(runDir); err != nil {
		return fmt.Errorf("%w: syncing %s: %w", ErrPending, runDir, err)
	}
	return settled(runDir)
}

// settled is ErrPending while any directory of a delivery is left in runDir.
// It looks at the disk itself, so a removal that only claims success does not
// pass for one.
func settled(runDir string) error {
	for _, name := range []string{liveName, stagingName, deletingName} {
		ok, err := present(filepath.Join(runDir, name))
		if err != nil {
			return fmt.Errorf("%w: %w", ErrPending, err)
		}
		if ok {
			return fmt.Errorf("%w: %s is left in %s", ErrPending, name, runDir)
		}
	}
	return nil
}

// Revoke takes from the harness the access to refs (to every ref, if refs is
// nil) of the delivery in runDir, for good: Resolver no longer returns them,
// and a Deliver of the same attempt keeps them revoked. Load still does, until
// Release. Without a delivery there is nothing to revoke.
func (m *Manager) Revoke(runDir string, refs []string) error {
	unlock, err := lock(runDir)
	if err != nil {
		return err
	}
	defer unlock()
	d, err := openDelivery(runDir)
	if errors.Is(err, ErrNotDelivered) {
		return nil
	}
	if err != nil {
		return err
	}
	man := d.m
	d.close()
	for i, e := range man.Refs {
		if refs == nil || slices.Contains(refs, e.Ref) {
			man.Refs[i].Harness = false
		}
	}
	data, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("secretjit: %w", err)
	}
	live := filepath.Join(runDir, liveName)
	tmp, final := filepath.Join(live, manifestTemp), filepath.Join(live, manifestName)
	if err := m.remove(tmp); err != nil {
		return err
	}
	if err := m.writeSynced(tmp, data); err != nil {
		return fmt.Errorf("secretjit: writing %s: %w", tmp, err)
	}
	if err := m.fs.rename(tmp, final); err != nil {
		return fmt.Errorf("secretjit: replacing %s: %w", final, err)
	}
	if err := m.fs.sync(live); err != nil {
		return fmt.Errorf("secretjit: syncing %s: %w", live, err)
	}
	return nil
}

// errHeld is a delivery Sweep leaves in place.
var errHeld = errors.New("secretjit: secrets still needed")

// Sweep is the janitor of the run directories in runsDir, named <run>-e<epoch>
// as domain.RunEpoch makes them; it ignores anything else. In each it removes
// what a crash left of a staging or a retired delivery, and releases the
// delivery in place when releasable says the Run no longer needs it and alive
// says no session of it (domain.SessionName) is running.
//
// Both are asked under the run's lock. When either cannot tell, it returns an
// error: the delivery is kept, as for a Run that needs it, and the error is
// part of Sweep's. Sweep goes through every run directory and joins the
// errors of all of them.
func (m *Manager) Sweep(runsDir string, releasable func(run string, epoch uint64) (bool, error), alive func(session string) (bool, error)) error {
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secretjit: %w", err)
	}
	var errs []error
	for _, e := range entries {
		run, epoch, ok := domain.ParseRunEpoch(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		verify := func() error {
			done, err := releasable(run, epoch)
			if err != nil {
				return fmt.Errorf("secretjit: is the Run done? %w", err)
			}
			if !done {
				return errHeld
			}
			live, err := alive(domain.SessionName(run, epoch))
			if err != nil {
				return fmt.Errorf("secretjit: is its session alive? %w", err)
			}
			if live {
				return errHeld
			}
			return nil
		}
		if err := m.sweep(filepath.Join(runsDir, e.Name()), verify); err != nil && !errors.Is(err, errHeld) {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// sweep releases runDir if it holds any directory of a delivery.
func (m *Manager) sweep(runDir string, verify func() error) error {
	if settled(runDir) == nil {
		return nil
	}
	return m.Release(runDir, verify)
}
