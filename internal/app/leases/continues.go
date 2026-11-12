package leases

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// HarnessVersionPrefix starts the capability with which a Worker announces the
// version of a harness it has: "harness:<id>:version:<version>". The
// Controller reads only its major, to decide whether the native state of a
// checkpoint can be restored there (§23.1).
func HarnessVersionPrefix(harness string) string {
	return domain.HarnessCapability(harness) + ":version:"
}

// continuationOf is what the assignment of a Run that continues a checkpoint
// carries, or nil for any other Run. The native state travels only if the
// harness of the Run is the adapter that wrote it and, when the Worker says
// which version it has, at the same major: otherwise the Run continues from the
// source and the summary alone, and the warnings say why (§23.1).
func continuationOf(ctx context.Context, q store.Querier, run store.Run, w store.Worker) (*v1.Continuation, error) {
	if run.LineageCheckpoint == "" {
		return nil, nil
	}
	cp, err := store.CheckpointByID(ctx, q, run.LineageCheckpoint)
	if err != nil {
		return nil, fmt.Errorf("leases: the checkpoint of run %s: %w", run.ID, err)
	}
	var spec struct {
		Requirements struct {
			Harness string `json:"harness"`
		} `json:"requirements"`
	}
	if err := json.Unmarshal(run.Spec, &spec); err != nil {
		return nil, fmt.Errorf("leases: reading the harness of a spec: %w", err)
	}
	out := &v1.Continuation{
		ParentRunID: string(run.LineageParent), CheckpointID: string(cp.ID), Ref: cp.SourceRef,
		Summary: v1.CheckpointSummary(cp.Summary),
	}
	native := cp.NativeState
	switch {
	case native == nil:
		out.Warnings = append(out.Warnings, "the checkpoint holds no native state: the Run continues from the source and the summary")
	case native.Adapter != spec.Requirements.Harness:
		out.Warnings = append(out.Warnings, fmt.Sprintf("the native state is of the %s adapter and the Run uses %q: the Run continues from the source and the summary",
			native.Adapter, spec.Requirements.Harness))
	case !sameMajor(native.AdapterVersion, w.Report.Capabilities, spec.Requirements.Harness):
		out.Warnings = append(out.Warnings, fmt.Sprintf("the native state was written by %s %s, another major version than the Worker's: the Run continues from the source and the summary",
			native.Adapter, native.AdapterVersion))
	default:
		out.NativeState = &v1.NativeStateRef{Adapter: native.Adapter, AdapterVersion: native.AdapterVersion, Digest: native.Digest}
	}
	return out, nil
}

// sameMajor says whether the harness the Worker announces is of the major of
// version. A Worker that announces no version, or one that cannot be read,
// is taken as compatible: its adapter checks again when it imports.
func sameMajor(version string, capabilities []string, harness string) bool {
	prefix := HarnessVersionPrefix(harness)
	i := slices.IndexFunc(capabilities, func(c string) bool { return strings.HasPrefix(c, prefix) })
	if i < 0 {
		return true
	}
	have, ok1 := majorOf(strings.TrimPrefix(capabilities[i], prefix))
	want, ok2 := majorOf(version)
	return !ok1 || !ok2 || have == want
}

// majorOf is the first number in a version ("v2.1.3", "2.1.3 (Claude Code)").
func majorOf(version string) (int, bool) {
	digits := strings.TrimLeftFunc(version, func(r rune) bool { return r < '0' || r > '9' })
	end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
	if end >= 0 {
		digits = digits[:end]
	}
	var n int
	if _, err := fmt.Sscanf(digits, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}
