package host

import (
	"context"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestAnnounce_CheckpointCapabilityIsThatOfAnOfferedHarnessWithNativeCheckpoint(t *testing.T) {
	headless := []domain.Mode{domain.ModeHeadless}
	native := adapter("native", headless, true, installed)
	native.caps.NativeCheckpoint = true
	missing := adapter("missing", headless, true, notInstalled)
	missing.caps.NativeCheckpoint = true
	plain := adapter("plain", headless, true, installed)
	cases := []struct {
		name     string
		adapters []harness.Adapter
		want     bool
	}{
		{"offered with native checkpoint", []harness.Adapter{native}, true},
		{"native checkpoint but not installed", []harness.Adapter{missing}, false},
		{"offered without native checkpoint", []harness.Adapter{plain}, false},
		{"one of several offers it", []harness.Adapter{plain, native}, true},
		{"no harness", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := harness.NewRegistry(tc.adapters...)
			if err != nil {
				t.Fatal(err)
			}
			h, _ := testHost(t)
			got := slices.Contains(Announce(context.Background(), h, reg, bothModes).Capabilities, domain.CheckpointCapability)
			if got != tc.want {
				t.Errorf("announces %s = %v, want %v", domain.CheckpointCapability, got, tc.want)
			}
		})
	}
}
