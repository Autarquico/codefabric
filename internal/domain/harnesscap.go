package domain

import (
	"slices"
	"strings"
)

// HarnessModesMarker is the capability of a Worker that announces, for each
// harness it has, every mode it runs it in. A Worker without it predates the
// announcement of modes (§10.1).
const HarnessModesMarker = "harness-modes:v1"

// WorkflowConfigCapability is that of a Worker that materialises the
// configuration of a Workflow version for the Runs that froze one. A Run with a
// Workflow is only placed on a Worker that announces it.
const WorkflowConfigCapability = "workflow-config:v1"

// legacyHarness is the only harness of the Workers that predate
// HarnessModesMarker, and legacyModes the modes it ran in there.
const legacyHarness = "command"

var legacyModes = []Mode{ModeHeadless, ModeInteractive}

// HarnessCapability says that a Worker has the harness in at least one mode.
func HarnessCapability(harness string) string { return "harness:" + harness }

// HarnessModeCapability says that a Worker runs the harness in the mode.
func HarnessModeCapability(harness string, mode Mode) string {
	return HarnessCapability(harness) + ":mode:" + string(mode)
}

// HarnessConfigIsolationCapability says that a Worker gives every Run of the
// harness a configuration of its own.
func HarnessConfigIsolationCapability(harness string) string {
	return HarnessCapability(harness) + ":config-isolation"
}

// HarnessModes is the modes a Worker runs the harness in, read from its
// capabilities, in the order it announces them. Without harness:<id> there
// are none. A Worker that predates HarnessModesMarker announces no modes: it
// is taken to run command in both, and no other harness at all.
func HarnessModes(capabilities []string, harness string) []Mode {
	modes := []Mode{}
	if !slices.Contains(capabilities, HarnessCapability(harness)) {
		return modes
	}
	if !slices.Contains(capabilities, HarnessModesMarker) {
		if harness == legacyHarness {
			modes = append(modes, legacyModes...)
		}
		return modes
	}
	prefix := HarnessModeCapability(harness, "")
	for _, c := range capabilities {
		if mode, ok := strings.CutPrefix(c, prefix); ok && mode != "" {
			modes = append(modes, Mode(mode))
		}
	}
	return modes
}
