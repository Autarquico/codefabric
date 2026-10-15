package domain

import (
	"slices"
	"testing"
)

func TestHarnessCapabilityNames(t *testing.T) {
	for got, want := range map[string]string{
		HarnessCapability("fake"):                      "harness:fake",
		HarnessModeCapability("fake", ModeHeadless):    "harness:fake:mode:headless",
		HarnessModeCapability("fake", ModeInteractive): "harness:fake:mode:interactive",
		HarnessConfigIsolationCapability("fake"):       "harness:fake:config-isolation",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

func TestHarnessModes(t *testing.T) {
	headless := HarnessModeCapability("fake", ModeHeadless)
	interactive := HarnessModeCapability("fake", ModeInteractive)
	for _, c := range []struct {
		name         string
		harness      string
		capabilities []string
		want         []Mode
	}{
		{"nothing announced", "fake", nil, []Mode{}},
		{"modes without the harness: an empty intersection", "fake", []string{HarnessModesMarker, headless}, []Mode{}},
		{"a partial announcement", "fake", []string{HarnessModesMarker, HarnessCapability("fake"), headless}, []Mode{ModeHeadless}},
		{"both modes", "fake", []string{HarnessModesMarker, HarnessCapability("fake"), interactive, headless}, []Mode{ModeInteractive, ModeHeadless}},
		{
			"another harness's modes do not count", "fake",
			[]string{HarnessModesMarker, HarnessCapability("fake"), HarnessModeCapability("other", ModeHeadless), HarnessConfigIsolationCapability("fake")},
			[]Mode{},
		},
		{"N with command announces its modes", "command", []string{HarnessModesMarker, HarnessCapability("command"), HarnessModeCapability("command", ModeHeadless)}, []Mode{ModeHeadless}},
		{"N-1 with command runs it in both modes", "command", []string{HarnessCapability("command")}, []Mode{ModeHeadless, ModeInteractive}},
		{"N-1 runs no other harness", "fake", []string{HarnessCapability("fake"), headless}, []Mode{}},
		{"N-1 without the harness", "command", []string{"docker"}, []Mode{}},
	} {
		got := HarnessModes(c.capabilities, c.harness)
		if got == nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %#v, want %v", c.name, got, c.want)
		}
	}
}
