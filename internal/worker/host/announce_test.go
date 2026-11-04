package host

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

var bothModes = []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}

func adapter(id string, modes []domain.Mode, isolated bool, detect func(context.Context) (harness.Detection, error)) *mini {
	return &mini{id: id, caps: harness.Capabilities{Modes: modes, ConfigIsolation: isolated}, detect: detect}
}

func installed(context.Context) (harness.Detection, error) {
	return harness.Detection{Installed: true, Path: "/opt/bin/x"}, nil
}

func notInstalled(context.Context) (harness.Detection, error) { return harness.Detection{}, nil }

func TestAnnounce(t *testing.T) {
	broken := errors.New("version probe crashed")
	cases := []struct {
		name         string
		adapters     []harness.Adapter
		runtimeModes []domain.Mode
		want         []string
		statuses     map[string]Status
	}{
		{
			name:         "no harness",
			runtimeModes: bothModes,
			want:         []string{domain.HarnessModesMarker},
			statuses:     map[string]Status{},
		},
		{
			name:         "not installed",
			adapters:     []harness.Adapter{adapter("fake", bothModes, true, notInstalled)},
			runtimeModes: bothModes,
			want:         []string{domain.HarnessModesMarker},
			statuses:     map[string]Status{"fake": NotInstalled},
		},
		{
			name: "detection failed",
			adapters: []harness.Adapter{adapter("fake", bothModes, true, func(context.Context) (harness.Detection, error) {
				return harness.Detection{Installed: true}, broken
			})},
			runtimeModes: bothModes,
			want:         []string{domain.HarnessModesMarker},
			statuses:     map[string]Status{"fake": DetectFailed},
		},
		{
			name: "detect unsupported is a failure, not an install",
			adapters: []harness.Adapter{adapter("bare", bothModes, false, func(context.Context) (harness.Detection, error) {
				return harness.Detection{}, harness.NewUnsupportedError("detect")
			})},
			runtimeModes: bothModes,
			want:         []string{domain.HarnessModesMarker},
			statuses:     map[string]Status{"bare": DetectFailed},
		},
		{
			name:         "empty intersection of modes",
			adapters:     []harness.Adapter{adapter("fake", []domain.Mode{domain.ModeInteractive}, true, installed)},
			runtimeModes: []domain.Mode{domain.ModeHeadless},
			want:         []string{domain.HarnessModesMarker},
			statuses:     map[string]Status{"fake": NoCommonMode},
		},
		{
			name:         "modes are those the Runtime runs too",
			adapters:     []harness.Adapter{adapter("fake", bothModes, false, installed)},
			runtimeModes: []domain.Mode{domain.ModeHeadless},
			want:         []string{"harness-modes:v1", "harness:fake", "harness:fake:mode:headless"},
			statuses:     map[string]Status{"fake": Offered},
		},
		{
			name:         "config isolation",
			adapters:     []harness.Adapter{adapter("fake", []domain.Mode{domain.ModeHeadless}, true, installed)},
			runtimeModes: bothModes,
			want:         []string{"harness-modes:v1", "harness:fake", "harness:fake:config-isolation", "harness:fake:mode:headless"},
			statuses:     map[string]Status{"fake": Offered},
		},
		{
			name: "sorted and without repeats, whatever the order of modes",
			adapters: []harness.Adapter{
				adapter("zeta", []domain.Mode{domain.ModeInteractive, domain.ModeHeadless, domain.ModeInteractive}, true, installed),
				adapter("alpha", []domain.Mode{domain.ModeHeadless, domain.ModeHeadless}, false, installed),
				adapter("gone", bothModes, true, notInstalled),
			},
			runtimeModes: []domain.Mode{domain.ModeInteractive, domain.ModeHeadless, domain.ModeHeadless},
			want: []string{
				"harness-modes:v1",
				"harness:alpha", "harness:alpha:mode:headless",
				"harness:zeta", "harness:zeta:config-isolation", "harness:zeta:mode:headless", "harness:zeta:mode:interactive",
			},
			statuses: map[string]Status{"alpha": Offered, "gone": NotInstalled, "zeta": Offered},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg, err := harness.NewRegistry(c.adapters...)
			if err != nil {
				t.Fatal(err)
			}
			h, _ := testHost(t)
			got := Announce(context.Background(), h, reg, c.runtimeModes)
			want := slices.Sorted(slices.Values(append(slices.Clone(c.want), domain.RepoConfigCapability, domain.WorkflowConfigCapability, domain.EnvironmentHostCapability)))
			if !slices.Equal(got.Capabilities, want) {
				t.Errorf("capabilities = %q, want %q", got.Capabilities, want)
			}
			if len(got.Harnesses) != len(c.statuses) {
				t.Errorf("harnesses = %+v, want %v", got.Harnesses, c.statuses)
			}
			ids := []string{}
			for _, hr := range got.Harnesses {
				ids = append(ids, hr.ID)
				if hr.Status != c.statuses[hr.ID] {
					t.Errorf("%s: status %s, want %s", hr.ID, hr.Status, c.statuses[hr.ID])
				}
				if (hr.Status == DetectFailed) != (hr.Err != nil) {
					t.Errorf("%s: status %s with err %v", hr.ID, hr.Status, hr.Err)
				}
				if hr.Status != Offered && len(hr.Modes) != 0 {
					t.Errorf("%s: not offered, with modes %v", hr.ID, hr.Modes)
				}
			}
			if !slices.IsSorted(ids) {
				t.Errorf("harnesses not by ID: %v", ids)
			}
		})
	}
}

func TestAnnounce_ReportsTheErrorAndPathOfDetect(t *testing.T) {
	broken := errors.New("version probe crashed")
	reg, err := harness.NewRegistry(
		adapter("bad", bothModes, false, func(context.Context) (harness.Detection, error) { return harness.Detection{}, broken }),
		adapter("good", bothModes, false, installed),
	)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := testHost(t)
	got := Announce(context.Background(), h, reg, bothModes)
	if !errors.Is(got.Harnesses[0].Err, broken) {
		t.Errorf("bad: err = %v, want %v", got.Harnesses[0].Err, broken)
	}
	if g := got.Harnesses[1]; g.Path != "/opt/bin/x" || !slices.Equal(g.Modes, bothModes) {
		t.Errorf("good = %+v", g)
	}
}

func TestAnnounce_DetectIsCutShortOnTheClockOfTheHost(t *testing.T) {
	h, fake := testHost(t)
	slow := adapter("slow", bothModes, false, func(ctx context.Context) (harness.Detection, error) {
		<-ctx.Done()
		return harness.Detection{}, ctx.Err()
	})
	reg, err := harness.NewRegistry(slow)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		if fake.BlockUntil(ctx, 1) == nil {
			fake.Advance(DetectTimeout)
		}
	}()
	got := Announce(ctx, h, reg, bothModes)
	if s := got.Harnesses[0]; s.Status != DetectFailed || !errors.Is(s.Err, errDetectTimeout) {
		t.Errorf("slow = %+v, want a detection that timed out", s)
	}
}

func TestAnnounce_DoesNotWaitForADetectThatIgnoresItsContext(t *testing.T) {
	h, fake := testHost(t)
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	deaf := adapter("deaf", bothModes, false, func(context.Context) (harness.Detection, error) {
		<-hung
		return harness.Detection{Installed: true}, nil
	})
	reg, err := harness.NewRegistry(deaf, adapter("good", bothModes, false, installed))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		if fake.BlockUntil(ctx, 1) == nil {
			fake.Advance(DetectTimeout)
		}
	}()
	got := Announce(ctx, h, reg, bothModes)
	if s := got.Harnesses[0]; s.Status != DetectFailed || !errors.Is(s.Err, errDetectTimeout) {
		t.Errorf("deaf = %+v, want a detection that timed out", s)
	}
	if s := got.Harnesses[1]; s.Status != Offered {
		t.Errorf("good = %+v, want offered", s)
	}
}
