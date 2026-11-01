package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

func doctored(t *testing.T, runtime func(context.Context) error) *daemon {
	t.Helper()
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	fake := clock.NewFake(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	return &daemon{
		clock: fake, doctorEvery: time.Minute,
		doctor: doctor.New(doctor.Options{
			Host: h, DataDir: t.TempDir(), Git: "true", Runtime: runtime,
			Disk: func(string) (sysinfo.Disk, error) { return sysinfo.Disk{FreeBytes: 50, TotalBytes: 100}, nil },
		}),
	}
}

func TestWithComponents_RunsTheDoctorOnItsIntervalNotOnEveryHeartbeat(t *testing.T) {
	runs := 0
	d := doctored(t, func(context.Context) error { runs++; return nil })
	for range 5 {
		if h := d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK}); h.Status != v1.HealthOK || len(h.Components) < 3 {
			t.Fatalf("health = %+v", h)
		}
	}
	if runs != 1 {
		t.Errorf("the doctor ran %d times in 5 heartbeats", runs)
	}
	d.clock.(*clock.Fake).Advance(time.Minute)
	d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK})
	if runs != 2 {
		t.Errorf("the doctor ran %d times after its interval", runs)
	}
}

func TestWithComponents_AFailingComponentDegradesTheWorker(t *testing.T) {
	d := doctored(t, func(context.Context) error { return errors.New("tmux: not available") })
	h := d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK})
	if h.Status != v1.HealthDegraded || h.Message == "" {
		t.Errorf("health = %+v", h)
	}
}

func TestWithComponents_KeepsTheReasonTheWorkerWasAlreadyDegradedFor(t *testing.T) {
	d := doctored(t, func(context.Context) error { return errors.New("no tmux") })
	h := d.withComponents(t.Context(), v1.Health{Status: v1.HealthDegraded, Message: "cannot measure this machine"})
	if h.Message != "cannot measure this machine" || len(h.Components) == 0 {
		t.Errorf("health = %+v", h)
	}
	last := h.Components[len(h.Components)-1]
	if last.Name != "daemon" || last.Status != v1.ComponentFail || last.Message != "cannot measure this machine" {
		t.Errorf("a degradation of the daemon must be a global failing component, got %+v", last)
	}
	for _, c := range d.components {
		if c.Name == "daemon" {
			t.Error("the component leaked into the cached result of the doctor")
		}
	}
}

func TestWithComponents_AHeartbeatTheControllerAccepts(t *testing.T) {
	d := doctored(t, func(context.Context) error { return errors.New("bad\nname " + strings.Repeat("x", 1000)) })
	h := d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK})
	for _, c := range h.Components {
		for _, s := range []string{c.Name, c.Message, c.Action, h.Message} {
			if len(s) > doctor.MaxText || strings.ContainsAny(s, "\n\r\t\x1b") {
				t.Errorf("component %q has a text the Controller rejects: %q", c.Name, s)
			}
		}
	}
}

func TestWithComponents_WithoutADoctorChangesNothing(t *testing.T) {
	in := v1.Health{Status: v1.HealthOK}
	d := &daemon{clock: clock.Real{}}
	if got := d.withComponents(t.Context(), in); got.Status != in.Status || got.Components != nil {
		t.Errorf("health = %+v", got)
	}
}

func TestWithComponents_ABlockedCheckEndsAtTheDeadlineOfTheInjectedClock(t *testing.T) {
	d := doctored(t, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	done := make(chan v1.Health, 1)
	go func() { done <- d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK}) }()
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := d.clock.(*clock.Fake).BlockUntil(waitCtx, 1); err != nil {
		t.Fatal(err)
	}
	d.clock.(*clock.Fake).Advance(doctorTimeout)
	select {
	case h := <-done:
		if h.Status != v1.HealthDegraded {
			t.Errorf("health = %+v, want degraded by the check that timed out", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the doctor did not end at its deadline on the injected clock")
	}
}

func TestWithDeadline_FollowsTheInjectedClock(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	ctx, cancel := withDeadline(t.Context(), clk, 15*time.Second)
	defer cancel()
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := clk.BlockUntil(waitCtx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(14 * time.Second)
	if ctx.Err() != nil {
		t.Fatal("cancelled before the deadline")
	}
	clk.Advance(time.Second)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled at the deadline")
	}
}

func TestControllerComponent(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	cases := []struct {
		name   string
		probe  func(context.Context) error
		status string
	}{
		{"not joined", nil, v1.ComponentWarn},
		{"answers", func(context.Context) error { return nil }, v1.ComponentOK},
		{"refuses", func(context.Context) error { return errors.New("certificate revoked") }, v1.ComponentFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CF_CONFIG_DIR", t.TempDir())
			t.Setenv("CF_DATA_DIR", t.TempDir())
			paths, err := config.ResolveFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			got := controllerComponent(t.Context(), clk, DoctorOptions{Paths: paths, Probe: c.probe}, "box")
			if got.Name != "controller" || got.Status != c.status || (c.status != v1.ComponentOK && got.Action == "") {
				t.Errorf("component = %+v, want %s", got, c.status)
			}
		})
	}
}

func TestControllerComponent_AControllerThatDoesNotAnswerFailsAtTheDeadline(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	done := make(chan v1.ComponentHealth, 1)
	go func() {
		done <- controllerComponent(t.Context(), clk, DoctorOptions{Probe: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}, "box")
	}()
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := clk.BlockUntil(waitCtx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(controllerTimeout)
	select {
	case got := <-done:
		if got.Status != v1.ComponentFail {
			t.Errorf("component = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe was not cut at the deadline")
	}
}

func TestWithComponents_ReportsSetupFailingWhileTheJoinIsPending(t *testing.T) {
	pending := true
	d := doctored(t, func(context.Context) error { return nil })
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	d.doctor = doctor.New(doctor.Options{
		Host: h, DataDir: t.TempDir(), Git: "true", Runtime: func(context.Context) error { return nil },
		Disk:         func(string) (sysinfo.Disk, error) { return sysinfo.Disk{FreeBytes: 50, TotalBytes: 100}, nil },
		SetupPending: func() bool { return pending },
	})
	got := d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK})
	var setup *v1.ComponentHealth
	for i, c := range got.Components {
		if c.Name == "setup" {
			setup = &got.Components[i]
		}
	}
	if got.Status != v1.HealthDegraded || setup == nil || setup.Status != v1.ComponentFail || setup.Action != "cf worker join --resume" {
		t.Fatalf("health = %+v", got)
	}
	pending = false
	d.clock.(*clock.Fake).Advance(time.Minute)
	if got := d.withComponents(t.Context(), v1.Health{Status: v1.HealthOK}); got.Status != v1.HealthOK {
		t.Fatalf("after the join: %+v", got)
	}
}

func TestRunDoctor_SetupComponentIsInThePublicDoctorButNotInTheWizards(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	if err := doctor.SaveSettings(paths, doctor.Settings{Name: "w1", SetupPending: true}); err != nil {
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	check := func(ignoreSetup bool) v1.ComponentHealth {
		t.Helper()
		components, err := RunDoctor(t.Context(), DoctorOptions{
			Paths: paths, IgnoreSetup: ignoreSetup, Probe: func(context.Context) error { return nil },
			Doctor: doctor.Options{
				Host: h, Adapters: []harness.Adapter{}, Git: "true", Runtime: func(context.Context) error { return nil },
				Disk: func(string) (sysinfo.Disk, error) { return sysinfo.Disk{FreeBytes: 50, TotalBytes: 100}, nil },
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range components {
			if c.Name == doctor.SetupComponent {
				return c
			}
		}
		return v1.ComponentHealth{}
	}
	if c := check(false); c.Status != v1.ComponentFail || !strings.Contains(c.Action, "--resume") {
		t.Errorf("the public doctor does not report the pending setup: %+v", c)
	}
	if c := check(true); c.Name != "" {
		t.Errorf("the wizard's doctor waits for itself: %+v", c)
	}
}
