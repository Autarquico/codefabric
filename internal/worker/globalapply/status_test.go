package globalapply

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

func TestApplied_SaysCleanDriftedAndMissingWithoutAWorkflow(t *testing.T) {
	eachKit(t, func(t *testing.T, w *world) {
		k := w.k
		if got, err := w.e.Applied(t.Context()); err != nil || len(got) != 0 {
			t.Fatalf("nothing applied = %+v, %v", got, err)
		}
		if _, err := w.apply(t, Request{Snapshot: k.snap(t, "v1", harness.Managed, k.doc)}); err != nil {
			t.Fatal(err)
		}
		state := func() Applied {
			t.Helper()
			got, err := w.e.Applied(t.Context())
			if err != nil || len(got) != 1 || got[0].Name != k.doc {
				t.Fatalf("applied = %+v, %v", got, err)
			}
			return got[0]
		}
		if a := state(); a.Drift || a.Missing || a.Ownership != harness.Managed {
			t.Errorf("clean = %+v", a)
		}
		w.write(t, k.docOnHost, "edited by hand\n")
		if a := state(); !a.Drift || a.Missing {
			t.Errorf("edited = %+v", a)
		}
		if err := os.Remove(filepath.Join(w.home, k.docOnHost)); err != nil {
			t.Fatal(err)
		}
		if a := state(); !a.Missing || a.Drift {
			t.Errorf("removed = %+v", a)
		}
	})
}
