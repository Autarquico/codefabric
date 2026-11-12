package executor

import (
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

func TestSnapshotOf_OnlyAFullCommitIDInTheFrozenSource(t *testing.T) {
	digest := strings.Repeat("a", 40)
	for name, tc := range map[string]struct{ spec, want string }{
		"a snapshot":        {`{"source":{"ref":"main","commit":"` + digest + `","snapshot":"` + digest + `"}}`, digest},
		"no snapshot":       {`{"source":{"ref":"main","snapshot":""}}`, ""},
		"not a commit":      {`{"source":{"snapshot":"refs/heads/main"}}`, ""},
		"a spec that fails": {`{`, ""},
	} {
		if got := snapshotOf(state.Run{Spec: []byte(tc.spec)}); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
