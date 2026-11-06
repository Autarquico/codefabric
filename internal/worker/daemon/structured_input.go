package daemon

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// withStructuredInput keeps the announcement of structured-input:v1 only if the
// permission bridge, which this binary is (`cf worker mcp-bridge`) or argv
// says, can be started: a Run that asks permissions cannot be answered
// without it. It returns the capabilities and the harnesses that remain
// announced, for the executor.
func (d *daemon) withStructuredInput(ctx context.Context, caps []string, reg *harness.Registry, argv []string) (kept, harnesses []string) {
	ready := bridgeInstalled(argv)
	suffix := ":" + domain.StructuredInputCapability
	kept = slices.DeleteFunc(slices.Clone(caps), func(c string) bool {
		if !strings.HasSuffix(c, suffix) {
			return false
		}
		if !ready {
			d.log.WarnContext(ctx, "structured input is not announced: the permission bridge cannot be started", "capability", c)
		}
		return !ready
	})
	if reg != nil {
		for _, id := range reg.IDs() {
			if domain.HasStructuredInput(kept, id) {
				harnesses = append(harnesses, id)
			}
		}
	}
	return kept, harnesses
}

// bridgeInstalled says whether the program that serves the permission tool
// exists: argv[0], or this binary.
func bridgeInstalled(argv []string) bool {
	path := ""
	if len(argv) > 0 {
		path = argv[0]
	} else if self, err := os.Executable(); err == nil {
		path = self
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0
}
