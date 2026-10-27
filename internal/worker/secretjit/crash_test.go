package secretjit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

// setup prepares a run directory before the operation under test.
type setup struct {
	name string
	prep func(t *testing.T, dir string)
}

func orphans(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{stagingName, deletingName} {
		if err := os.MkdirAll(filepath.Join(dir, name, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "sub", "left"), []byte("leftover-value"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// recovery is what runs after a fault, with the real file system, and the
// state it must converge to.
type recovery struct {
	name string
	do   func(dir string) error
	want []redact.Needle // nil: nothing is left
}

func sweepAll(dir string) error {
	return New().Sweep(filepath.Dir(dir),
		func(string, uint64) (bool, error) { return true, nil },
		func(string) (bool, error) { return false, nil })
}

var recoveries = []recovery{
	{name: "deliver again", do: func(dir string) error { return New().Deliver(dir, 2, v2, allow) }, want: v2},
	{name: "release", do: func(dir string) error { return New().Release(dir, allow) }},
	{name: "sweep", do: sweepAll},
}

// converges checks that rec, after whatever a fault left, ends in its state.
func converges(t *testing.T, dir string, rec recovery) {
	t.Helper()
	if err := rec.do(dir); err != nil {
		t.Fatalf("%s: %v", rec.name, err)
	}
	if rec.want == nil {
		wantOnly(t, dir)
		return
	}
	wantOnly(t, dir, liveName)
	wantLoad(t, dir, rec.want)
	wantPrivate(t, dir)
}

// steps is what op does on a clean run of the setup.
func steps(t *testing.T, s setup, op func(m *Manager, dir string) error) []string {
	t.Helper()
	dir := runDir(t)
	s.prep(t, dir)
	f := &faults{}
	if err := op(f.manager(), dir); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	return f.steps
}

// eachFault runs op with a fault at every step, in every mode, and then each
// recovery on what is left, after check has looked at it.
func eachFault(t *testing.T, s setup, op func(m *Manager, dir string) error, check func(t *testing.T, dir string, crashed bool, err error)) {
	all := steps(t, s, op)
	for at, step := range all {
		for _, md := range modes {
			for _, rec := range recoveries {
				t.Run(fmt.Sprintf("%s/%02d %s/%s/%s", s.name, at+1, step, md.name, rec.name), func(t *testing.T) {
					t.Parallel()
					dir := runDir(t)
					s.prep(t, dir)
					f := &faults{at: at + 1, mode: md}
					crashed, err := run(func() error { return op(f.manager(), dir) })
					if !crashed && err == nil {
						t.Fatalf("a fault at %q went unreported", step)
					}
					noValue(t, err)
					check(t, dir, crashed, err)
					converges(t, dir, rec)
				})
			}
		}
	}
}

func TestDeliver_FaultAtEveryStepLeavesNoHalfDeliveryAndRecovers(t *testing.T) {
	setups := []setup{
		{name: "fresh", prep: func(*testing.T, string) {}},
		{name: "replacing", prep: func(t *testing.T, dir string) { deliver(t, dir, 1, v1) }},
		{name: "after a crash", prep: func(t *testing.T, dir string) { deliver(t, dir, 1, v1); orphans(t, dir) }},
	}
	op := func(m *Manager, dir string) error { return m.Deliver(dir, 2, v2, allow) }
	for _, s := range setups {
		eachFault(t, s, op, func(t *testing.T, dir string, _ bool, _ error) {
			// secrets/ is absent, or a whole delivery: the old one or the new.
			got, err := Load(dir)
			switch {
			case errors.Is(err, ErrNotDelivered):
			case err != nil:
				t.Fatalf("after the fault, secrets/ is not a whole delivery: %v", err)
			case !sameNeedles(got, v2) && (s.name == "fresh" || !sameNeedles(got, v1)):
				t.Fatalf("after the fault, Load = %q", got)
			}
		})
	}
}

func TestRelease_FaultAtEveryStepIsPendingAndRecovers(t *testing.T) {
	setups := []setup{
		{name: "delivered", prep: func(t *testing.T, dir string) { deliver(t, dir, 1, v1) }},
		{name: "with orphans", prep: func(t *testing.T, dir string) { deliver(t, dir, 1, v1); orphans(t, dir) }},
		{name: "only orphans", prep: orphans},
		{name: "corrupt manifest", prep: func(t *testing.T, dir string) {
			deliver(t, dir, 1, v1)
			if err := os.WriteFile(filepath.Join(dir, liveName, manifestName), []byte("{nope"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no manifest", prep: func(t *testing.T, dir string) {
			deliver(t, dir, 1, v1)
			if err := os.Remove(filepath.Join(dir, liveName, manifestName)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	op := func(m *Manager, dir string) error { return m.Release(dir, allow) }
	for _, s := range setups {
		eachFault(t, s, op, func(t *testing.T, dir string, crashed bool, err error) {
			if !crashed && !errors.Is(err, ErrPending) {
				t.Fatalf("Release = %v, want ErrPending", err)
			}
			// secrets/ is never left half removed.
			if s.name == "delivered" || s.name == "with orphans" {
				if _, err := Load(dir); err != nil && !errors.Is(err, ErrNotDelivered) {
					t.Fatalf("after the fault, secrets/ is not a whole delivery: %v", err)
				}
			}
		})
	}
}

func TestRevoke_FaultAtEveryStepLeavesAccessWholeOrRevokedAndRecovers(t *testing.T) {
	s := setup{name: "delivered", prep: func(t *testing.T, dir string) { deliver(t, dir, 1, v1) }}
	op := func(m *Manager, dir string) error { return m.Revoke(dir, nil) }
	eachFault(t, s, op, func(t *testing.T, dir string, _ bool, _ error) {
		wantLoad(t, dir, v1)
		resolve := Resolver(dir)
		_, err0 := resolve(v1[0].Ref)
		_, err1 := resolve(v1[1].Ref)
		if (err0 == nil) != (err1 == nil) {
			t.Fatalf("access half revoked: %v, %v", err0, err1)
		}
		if err0 != nil && !errors.Is(err0, ErrNoAccess) {
			t.Fatalf("resolve = %v", err0)
		}
		// Revoking again finishes it.
		if err := New().Revoke(dir, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(v1[0].Ref); !errors.Is(err, ErrNoAccess) {
			t.Fatalf("resolve after Revoke = %v", err)
		}
	})
}

func TestRelease_ARemovalThatOnlyClaimsSuccessIsPending(t *testing.T) {
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	m := New()
	m.fs.removeAll = func(string) error { return nil }
	if err := m.Release(dir, allow); !errors.Is(err, ErrPending) {
		t.Fatalf("Release = %v, want ErrPending", err)
	}
	if err := New().Release(dir, allow); err != nil {
		t.Fatal(err)
	}
	wantOnly(t, dir)
}

func TestRelease_RemovesWhatTheHarnessMadeUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := runDir(t)
	deliver(t, dir, 1, v1)
	sub := filepath.Join(dir, liveName, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{sub, filepath.Join(dir, liveName)} {
		if err := chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := New().Release(dir, allow); err != nil {
		t.Fatal(err)
	}
	wantOnly(t, dir)
}
