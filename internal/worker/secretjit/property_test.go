package secretjit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

// The property of the delivery of #23 (§11.4, §11.9): any sequence of
// deliveries (at times of an attempt older than the one in place, as a grant
// answered late would make), revocations, releases the caller refuses or
// allows, corrupt manifests and faults or crashes at any step of the disk
// leaves a run directory that a model says: secrets/ is absent or one whole
// delivery, the newest attempt; a release the caller refuses keeps it (the
// retention); a revocation takes the access and keeps the values; and once a
// release goes through without a fault, nothing is left.
//
// CF_SECRETS_SEED replays one seed; CF_SECRETS_SEEDS runs that many from
// CF_SECRETS_FROM (1).

// jitModel is what the run directory holds as the model has it.
type jitModel struct {
	attempt uint64 // of the delivery in place; 0 for none
	needles []redact.Needle
	access  bool // the harness may read them
	corrupt bool // the test broke its manifest
}

func jitSeeds(t *testing.T, def uint64) []uint64 {
	t.Helper()
	if text := os.Getenv("CF_SECRETS_SEED"); text != "" {
		seed, err := strconv.ParseUint(text, 0, 64)
		if err != nil {
			t.Fatalf("CF_SECRETS_SEED=%q is not a seed", text)
		}
		return []uint64{seed}
	}
	n, from := def, uint64(1)
	for name, into := range map[string]*uint64{"CF_SECRETS_SEEDS": &n, "CF_SECRETS_FROM": &from} {
		if text := os.Getenv(name); text != "" {
			v, err := strconv.ParseUint(text, 0, 64)
			if err != nil || v == 0 {
				t.Fatalf("%s=%q is not a positive number", name, text)
			}
			*into = v
		}
	}
	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = from + uint64(i)
	}
	return seeds
}

// jitWorld is a run directory, its model, and the needles of each attempt.
type jitWorld struct {
	t        *testing.T
	rng      *rand.Rand
	dir      string
	model    jitModel
	byTry    map[uint64][]redact.Needle
	next     uint64
	values   [][]byte
	log      []string
	replayOf string
}

func (w *jitWorld) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("%s\nactions: %s\nFAILED; replay: %s", fmt.Sprintf(format, args...), strings.Join(w.log, "; "), w.replayOf)
}

// needlesOf is the needles attempt delivers, drawn once.
func (w *jitWorld) needlesOf(attempt uint64) []redact.Needle {
	if n, ok := w.byTry[attempt]; ok {
		return n
	}
	var out []redact.Needle
	for _, ref := range []string{"api/token", "db.password", "gh/token"} {
		if w.rng.IntN(2) == 0 || (ref == "gh/token" && len(out) == 0) {
			value := fmt.Appendf(nil, "v-%016x%08x", w.rng.Uint64(), w.rng.Uint32())
			w.values = append(w.values, value)
			out = append(out, redact.Needle{Ref: ref, Value: value})
		}
	}
	w.byTry[attempt] = out
	return out
}

// faulty is a Manager that fails or crashes at a step drawn by the seed, or
// a sound one.
func (w *jitWorld) faulty() (*Manager, *faults) {
	if w.rng.IntN(3) != 0 {
		return New(), nil
	}
	f := &faults{at: 1 + w.rng.IntN(12), mode: modes[w.rng.IntN(len(modes))]}
	return f.manager(), f
}

// struck says whether an injected fault struck the operation.
func struck(f *faults, crashed bool, err error) bool {
	return f != nil && (crashed || errors.Is(err, errInjected))
}

// observe is what the run directory holds: the needles in place, if whole.
func (w *jitWorld) observe() (needles []redact.Needle, delivered bool) {
	got, err := Load(w.dir)
	switch {
	case errors.Is(err, ErrNotDelivered):
		return nil, false
	case err != nil:
		w.fail("secrets/ is not one whole delivery: %v", err)
	}
	return got, true
}

func sortedNeedles(n []redact.Needle) []redact.Needle {
	out := slices.Clone(n)
	slices.SortFunc(out, func(a, b redact.Needle) int { return strings.Compare(a.Ref, b.Ref) })
	return out
}

// settleTo makes the model what the disk shows after a fault, which must be
// one of the states the operation may have left.
func (w *jitWorld) settleTo(allowed ...jitModel) {
	got, err := Load(w.dir)
	for _, m := range allowed {
		switch {
		case m.corrupt:
			if err != nil && !errors.Is(err, ErrNotDelivered) {
				w.model = m
				return
			}
		case m.attempt == 0:
			if errors.Is(err, ErrNotDelivered) {
				w.model = m
				return
			}
		case err == nil && sameNeedles(sortedNeedles(got), sortedNeedles(m.needles)) && w.hasAccess(got) == m.access:
			w.model = m
			return
		}
	}
	w.fail("after a fault the delivery is %q (%v), which no state it may leave is: %+v", got, err, allowed)
}

// hasAccess says whether the harness may read needles, all of them or none.
func (w *jitWorld) hasAccess(needles []redact.Needle) bool {
	resolve := Resolver(w.dir)
	var reads int
	for _, n := range needles {
		v, err := resolve(n.Ref)
		switch {
		case err == nil && v == string(n.Value):
			reads++
		case err == nil:
			w.fail("the harness reads %q as another value", n.Ref)
		case !errors.Is(err, ErrNoAccess):
			w.fail("the harness reading %q: %v", n.Ref, err)
		}
	}
	if reads != 0 && reads != len(needles) {
		w.fail("the harness may read %d of %d secrets", reads, len(needles))
	}
	return reads == len(needles)
}

// check is the delivery in place against the model.
func (w *jitWorld) check() {
	w.t.Helper()
	if w.model.corrupt {
		if _, err := Load(w.dir); err == nil {
			w.fail("a corrupt manifest loads")
		}
		return
	}
	got, delivered := w.observe()
	switch {
	case delivered != (w.model.attempt != 0):
		w.fail("delivered = %v, the model has attempt %d", delivered, w.model.attempt)
	case delivered && !sameNeedles(sortedNeedles(got), sortedNeedles(w.model.needles)):
		w.fail("Load = %q, the model has attempt %d", got, w.model.attempt)
	case delivered && w.hasAccess(got) != w.model.access:
		w.fail("the harness may read the secrets: %v, the model %v", !w.model.access, w.model.access)
	}
}

func (w *jitWorld) step() {
	w.t.Helper()
	m, f := w.faulty()
	before := w.model
	switch action := w.rng.IntN(6); action {
	case 0, 1:
		attempt := w.next
		if w.model.attempt > 1 && w.rng.IntN(4) == 0 {
			attempt = 1 + w.rng.Uint64N(w.model.attempt-1) // a grant answered late
		} else {
			w.next++
		}
		needles := w.needlesOf(attempt)
		refuse := w.rng.IntN(6) == 0 // the Run moved on while it was prepared
		verify := allow
		if refuse {
			verify = func() error { return errors.New("the Run moved on") }
		}
		w.log = append(w.log, fmt.Sprintf("deliver %d refuse=%v fault=%v", attempt, refuse, f != nil))
		crashed, err := run(func() error { return m.Deliver(w.dir, attempt, needles, verify) })
		noValue(w.t, err)
		fresh := jitModel{attempt: attempt, needles: needles, access: true}
		stale := !before.corrupt && before.attempt > attempt
		switch {
		case struck(f, crashed, err):
			if stale || refuse {
				w.settleTo(before, jitModel{})
			} else {
				w.settleTo(before, fresh, jitModel{})
			}
		case refuse:
			if err == nil {
				w.fail("a delivery its caller refused went through")
			}
		case stale:
			if !errors.Is(err, ErrStale) {
				w.fail("an attempt %d older than %d in place: %v", attempt, before.attempt, err)
			}
		case err != nil:
			w.fail("Deliver(%d): %v", attempt, err)
		case before.attempt == attempt && !before.corrupt:
			// the same attempt again: it keeps what it was, revoked or not
		default:
			w.model = fresh
		}
	case 2:
		w.log = append(w.log, fmt.Sprintf("revoke fault=%v", f != nil))
		crashed, err := run(func() error { return m.Revoke(w.dir, nil) })
		noValue(w.t, err)
		switch {
		case before.corrupt:
		case struck(f, crashed, err):
			revoked := before
			revoked.access = false
			w.settleTo(before, revoked)
		case err != nil:
			w.fail("Revoke: %v", err)
		default:
			w.model.access = false
		}
	case 3, 4:
		refuse := w.rng.IntN(2) == 0 // the capture or the publication is not closed
		verify := allow
		if refuse {
			verify = func() error { return errors.New("still needed") }
		}
		w.log = append(w.log, fmt.Sprintf("release refuse=%v fault=%v", refuse, f != nil))
		crashed, err := run(func() error { return m.Release(w.dir, verify) })
		noValue(w.t, err)
		switch {
		case struck(f, crashed, err):
			if refuse {
				w.settleTo(before)
			} else {
				w.settleTo(before, jitModel{})
			}
		case refuse && before.attempt != 0:
			if err == nil {
				w.fail("a release its caller refused went through")
			}
		case err != nil && !refuse:
			w.fail("Release: %v", err)
		case !refuse:
			w.model = jitModel{}
		}
	case 5:
		if before.attempt != 0 && !before.corrupt && w.rng.IntN(2) == 0 {
			w.log = append(w.log, "corrupt")
			if err := os.WriteFile(filepath.Join(w.dir, liveName, manifestName), []byte("{"), 0o600); err != nil {
				w.t.Fatal(err)
			}
			w.model.corrupt = true
		}
	}
	w.check()
}

func TestProperty_SecretsDelivery(t *testing.T) {
	name := t.Name()
	for _, seed := range jitSeeds(t, 20) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			w := &jitWorld{
				t: t, rng: rand.New(rand.NewPCG(seed, seed^0xa11ce)), dir: runDir(t), byTry: map[uint64][]redact.Needle{}, next: 1, //nolint:gosec // a seed to replay, not a secret
				replayOf: fmt.Sprintf("CF_SECRETS_SEED=%d go test -race -count=1 -run '^%s$' ./internal/worker/secretjit/", seed, name),
			}
			t.Logf("seed %d (replay: %s)", seed, w.replayOf)
			for range 60 {
				w.step()
			}
			// The release that goes through leaves nothing, and no value anywhere.
			if err := New().Release(w.dir, allow); err != nil {
				w.fail("the last Release: %v", err)
			}
			wantOnly(t, w.dir)
			_ = filepath.WalkDir(filepath.Dir(w.dir), func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil //nolint:nilerr // only files are looked at
				}
				raw, _ := os.ReadFile(path) //nolint:gosec // a file of the test
				for _, v := range w.values {
					if bytes.Contains(raw, v) {
						w.fail("%s holds a value after the release", path)
					}
				}
				return nil
			})
		})
	}
}
