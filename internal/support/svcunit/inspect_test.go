package svcunit_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

func TestInspect_ReadsTheArgumentsAndEnvironmentBackWhateverTheQuoting(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			var e svcunit.Env
			if goos == "linux" {
				e, _ = newSystemd(t)
			} else {
				e, _ = newLaunchd(t)
			}
			install(t, e, workerSpec())
			in, err := svcunit.Inspect(t.Context(), e, workerSpec())
			if err != nil || !in.Found || !in.Active {
				t.Fatalf("%+v %v", in, err)
			}
			if !slices.Equal(in.Argv, workerSpec().Argv) {
				t.Errorf("argv = %q", in.Argv)
			}
			if in.Env["CF_DATA_DIR"] != awkward {
				t.Errorf("env = %q", in.Env)
			}
		})
	}
}

func TestInspect_NotInstalledAndNothingChanged(t *testing.T) {
	e, fake := newSystemd(t)
	in, err := svcunit.Inspect(t.Context(), e, workerSpec())
	if err != nil || in.Found || len(fake.Calls()) != 0 {
		t.Errorf("%+v %v %v", in, err, fake.Calls())
	}
}

func TestStopAndStart_LeaveTheFileAndOnlyTheRunningStateChange(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			var e svcunit.Env
			if goos == "linux" {
				e, _ = newSystemd(t)
			} else {
				e, _ = newLaunchd(t)
			}
			install(t, e, workerSpec())
			res, err := svcunit.Stop(t.Context(), e, workerSpec())
			if err != nil || !res.Changed {
				t.Fatalf("stop: %+v %v", res, err)
			}
			for _, a := range res.Actions {
				if strings.HasPrefix(a, "remove") || strings.Contains(a, "disable") {
					t.Errorf("stop did %q", a)
				}
			}
			if in, _ := svcunit.Inspect(t.Context(), e, workerSpec()); !in.Found || in.Active {
				t.Errorf("after stop: %+v", in)
			}
			if res, err := svcunit.Stop(t.Context(), e, workerSpec()); err != nil || res.Changed {
				t.Errorf("stopping what is stopped: %+v %v", res, err)
			}
			if res, err := svcunit.Start(t.Context(), e, workerSpec()); err != nil || !res.Changed {
				t.Fatalf("start: %+v %v", res, err)
			}
			if in, _ := svcunit.Inspect(t.Context(), e, workerSpec()); !in.Active {
				t.Errorf("after start: %+v", in)
			}
		})
	}
}

func TestReadPlist_NamesTheUserOfALaunchDaemon(t *testing.T) {
	info, err := svcunit.ReadPlist([]byte(`<plist><dict><key>UserName</key><string>ana</string>
<key>ProgramArguments</key><array><string>/bin/cf</string><string>worker</string></array>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>/Users/ana</string></dict></dict></plist>`))
	if err != nil || info.UserName != "ana" || !slices.Equal(info.Argv, []string{"/bin/cf", "worker"}) || info.Env["HOME"] != "/Users/ana" {
		t.Errorf("%+v %v", info, err)
	}
}
