package host

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

type versioned struct {
	*mini
	version string
}

func (v versioned) Version(context.Context, harness.Host) (string, error) { return v.version, nil }

func TestAnnounce_VersionOfAnOfferedHarness(t *testing.T) {
	const prefix = "harness:ver:version:"
	cases := []struct {
		name, version, want string
	}{
		{"a version", "2.1.3", prefix + "2.1.3"},
		{"a version with a suffix", "0.5.0-beta.1", prefix + "0.5.0-beta.1"},
		{"what is no token is left out", "2.1.3 (Claude Code)", ""},
		{"nothing", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := versioned{adapter("ver", bothModes, true, installed), tc.version}
			reg, err := harness.NewRegistry(a)
			if err != nil {
				t.Fatal(err)
			}
			h, _ := testHost(t)
			caps := Announce(context.Background(), h, reg, bothModes).Capabilities
			if !slices.Contains(caps, domain.HarnessCapability("ver")) {
				t.Errorf("harness not announced: %v", caps)
			}
			var got string
			for _, c := range caps {
				if strings.HasPrefix(c, prefix) {
					got = c
				}
			}
			if got != tc.want {
				t.Errorf("version capability = %q, want %q", got, tc.want)
			}
		})
	}
}
