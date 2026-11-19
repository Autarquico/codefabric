package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/support/svcunit"
	"github.com/Autarquico/codeflock/internal/support/svcunit/svcunittest"
)

type rig struct {
	home, cfg, data string
	fleet           *svcunittest.SystemdFleet
	env             svcunit.Env
}

var (
	oldWorker = svcunit.Spec{Name: "cf-worker", Label: "com.codefabric.worker", KeepProcesses: true}
	newWorker = svcunit.Spec{Name: "codeflock-worker", Label: "com.codeflock.worker", KeepProcesses: true}
)

// newRig is a user with a Worker service of the old name, installed and running,
// and the directories it uses, with a file of each that must survive.
func newRig(t *testing.T) *rig {
	t.Helper()
	home := t.TempDir()
	r := &rig{home: home, cfg: filepath.Join(home, ".config", "codefabric"), data: filepath.Join(home, ".local", "share", "codefabric")}
	r.fleet = svcunittest.NewSystemdFleet(home)
	r.env = svcunit.Env{Runner: r.fleet, Home: home, UID: 1000, GOOS: "linux"}
	tree(t, filepath.Dir(r.cfg), "codefabric", "enrolment")
	tree(t, filepath.Dir(r.data), "codefabric", "database")
	r.installOld(t, oldWorker, true)
	return r
}

func (r *rig) installOld(t *testing.T, spec svcunit.Spec, active bool) {
	t.Helper()
	spec.Argv = []string{"/opt/cf/bin/cf", "worker", "run", "--ssh-target", "me@host", "--odd arg"}
	spec.Env = []svcunit.KV{
		{Key: "CF_CONFIG_DIR", Value: r.cfg},
		{Key: "CF_DATA_DIR", Value: r.data},
		{Key: "PATH", Value: "/usr/bin"},
		{Key: "TMUX_TMPDIR", Value: "/run/tmux"},
	}
	spec.Installer = "cf worker install-service"
	if _, err := svcunit.Install(t.Context(), r.env, spec); err != nil {
		t.Fatal(err)
	}
	r.fleet.Set(spec.Name+".service", true, active)
}

func (r *rig) service() Service {
	return Service{
		Env: r.env, Old: oldWorker,
		Settle: func(context.Context) error { return nil },
		New: func(was Snapshot, dirs Dirs) (svcunit.Spec, error) {
			spec := newWorker
			spec.Argv = append([]string{"/opt/cflk/bin/cflk"}, was.Argv[1:]...)
			spec.Env = []svcunit.KV{
				{Key: "CFLK_CONFIG_DIR", Value: dirs.NewConfig},
				{Key: "CFLK_DATA_DIR", Value: dirs.NewData},
				{Key: "TMUX_TMPDIR", Value: was.Env["TMUX_TMPDIR"]},
			}
			spec.Installer = "cflk worker install-service"
			return spec, nil
		},
	}
}

func (r *rig) unit(name string) string {
	return filepath.Join(r.home, ".config", "systemd", "user", name+".service")
}

// unchanged says the machine is as newRig left it: the old service installed
// and running, the new one not there, every directory where it was.
func (r *rig) unchanged(t *testing.T) {
	t.Helper()
	if !has(t, r.unit("cf-worker")) || !r.fleet.Active("cf-worker.service") {
		t.Errorf("the old service is not installed and running")
	}
	if has(t, r.unit("codeflock-worker")) {
		t.Errorf("the new service is there")
	}
	for dir, content := range map[string]string{r.cfg: "enrolment", r.data: "database"} {
		if b, err := readFile(filepath.Join(dir, "f")); err != nil || string(b) != content {
			t.Errorf("%s: %q %v", dir, b, err)
		}
	}
	if has(t, filepath.Join(filepath.Dir(r.cfg), "codeflock")) || has(t, filepath.Join(filepath.Dir(r.data), "codeflock")) {
		t.Errorf("a new directory is left")
	}
	if has(t, filepath.Join(r.data, progressName)) {
		t.Errorf("the progress file is left")
	}
}

func TestServiceMigratesEverythingAndTheNewOneRuns(t *testing.T) {
	r := newRig(t)
	res, err := r.service().Migrate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if has(t, r.unit("cf-worker")) || r.fleet.Active("cf-worker.service") || !r.fleet.Active("codeflock-worker.service") {
		t.Errorf("old: %v, old active: %v, new active: %v", has(t, r.unit("cf-worker")), r.fleet.Active("cf-worker.service"), r.fleet.Active("codeflock-worker.service"))
	}
	newCfg, newData := filepath.Join(r.home, ".config", "codeflock"), filepath.Join(r.home, ".local", "share", "codeflock")
	for dir, content := range map[string]string{newCfg: "enrolment", newData: "database"} {
		if b, err := readFile(filepath.Join(dir, "f")); err != nil || string(b) != content {
			t.Errorf("%s: %q %v", dir, b, err)
		}
		if has(t, filepath.Join(dir, progressName)) {
			t.Errorf("progress left in %s", dir)
		}
	}
	got, _, _ := svcunit.InstalledEnv(r.env, newWorker)
	if got["CFLK_DATA_DIR"] != newData || got["TMUX_TMPDIR"] != "/run/tmux" {
		t.Errorf("env of the new unit: %v", got)
	}
	in, _ := svcunit.Inspect(t.Context(), r.env, newWorker)
	if strings.Join(in.Argv[1:], "|") != "worker|run|--ssh-target|me@host|--odd arg" {
		t.Errorf("arguments: %q", in.Argv)
	}
	if !res.Changed || len(res.Actions) == 0 {
		t.Errorf("%+v", res)
	}
}

func TestServiceCompensatesAFailureAtEveryStepAndTheOldOneRunsAgain(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		rig  func(*rig, *Service)
	}{
		{"stopping the old one", func(r *rig, _ *Service) { r.fleet.Fail("systemctl --user stop cf-worker", boom) }},
		{"moving the second directory", func(_ *rig, s *Service) {
			s.Mover.Rename = func(oldPath, newPath string) error {
				if strings.HasSuffix(newPath, filepath.Join("share", "codeflock")) {
					return boom
				}
				return renameNoReplace(oldPath, newPath)
			}
		}},
		{"starting the new one", func(r *rig, _ *Service) { r.fleet.Fail("systemctl --user start codeflock-worker", boom) }},
		{"the new one does not stay up", func(_ *rig, s *Service) {
			s.Settle = func(context.Context) error { return boom }
		}},
		{"removing the old one", func(r *rig, _ *Service) { r.fleet.Fail("systemctl --user disable --no-reload cf-worker", boom) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			s := r.service()
			c.rig(r, &s)
			if _, err := s.Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("err = %v", err)
			}
			r.fleet.Fail("systemctl --user disable --no-reload cf-worker", nil)
			r.unchanged(t)
		})
	}
}

func TestServiceThatCannotBePutBackKeepsTheProgressAndGoesOnWhenRunAgain(t *testing.T) {
	r := newRig(t)
	s := r.service()
	boom := errors.New("boom")
	r.fleet.Fail("systemctl --user start codeflock-worker", boom)
	r.fleet.Fail("systemctl --user start cf-worker", boom) // the old one cannot be put back either
	_, err := s.Migrate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "putting the old service back failed") {
		t.Fatalf("err = %v", err)
	}
	if !has(t, filepath.Join(r.data, progressName)) {
		t.Fatal("no progress file to resume from")
	}
	// The old service is stopped by now, and the second run has to know it was running.
	r.fleet.Fail("systemctl --user start codeflock-worker", nil)
	r.fleet.Fail("systemctl --user start cf-worker", nil)
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !r.fleet.Active("codeflock-worker.service") || has(t, r.unit("cf-worker")) {
		t.Errorf("not resumed: new active %v", r.fleet.Active("codeflock-worker.service"))
	}
}

func TestServiceResumesFromTheProgressFileAfterACut(t *testing.T) {
	r := newRig(t)
	// The cut: old service stopped, directories moved, nothing else done.
	snap := Snapshot{
		Active: true,
		Env:    map[string]string{"CF_CONFIG_DIR": r.cfg, "CF_DATA_DIR": r.data, "TMUX_TMPDIR": "/run/tmux"},
		Argv:   []string{"/opt/cf/bin/cf", "worker", "run"},
	}
	r.fleet.Set("cf-worker.service", true, false)
	newData := filepath.Join(r.home, ".local", "share", "codeflock")
	if _, err := Move([]Pair{{r.cfg, filepath.Join(r.home, ".config", "codeflock")}, {r.data, newData}}); err != nil {
		t.Fatal(err)
	}
	if err := (progressFile{dirs: Dirs{OldData: r.data, NewData: newData}}).write(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service().Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !r.fleet.Active("codeflock-worker.service") || has(t, r.unit("cf-worker")) || has(t, filepath.Join(newData, progressName)) {
		t.Errorf("not finished")
	}
}

func TestServiceStoppedStaysStoppedAndASuffixedOneIsFound(t *testing.T) {
	r := newRig(t)
	r.fleet.Set("cf-worker.service", true, false)
	if _, err := r.service().Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.fleet.Active("codeflock-worker.service") || !has(t, r.unit("codeflock-worker")) {
		t.Errorf("a stopped Worker must be installed and stopped")
	}
}

func TestServiceWithASuffixKeepsItsSuffix(t *testing.T) {
	r := newRig(t)
	suffixed := svcunit.Spec{Name: "cf-worker-t1", Label: "com.codefabric.worker-t1", KeepProcesses: true}
	r.installOld(t, suffixed, true)
	s := r.service()
	s.Old = suffixed
	// Both old units run on the same directories and the other one is running.
	if _, err := s.Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "cf-worker") {
		t.Fatalf("err = %v", err)
	}
	r.fleet.Set("cf-worker.service", true, false)
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if has(t, r.unit("cf-worker-t1")) || !has(t, r.unit("cf-worker")) {
		t.Errorf("only the named one moves")
	}
}

func TestServiceWithNoOldUnitSaysSo(t *testing.T) {
	r := newRig(t)
	if err := os.Remove(r.unit("cf-worker")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service().Migrate(t.Context()); !errors.Is(err, ErrNoOldService) {
		t.Errorf("err = %v", err)
	}
}

func TestServiceWithAnUnreadableUnitChangesNothing(t *testing.T) {
	r := newRig(t)
	if err := os.Remove(r.unit("cf-worker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.unit("cf-worker"), 0o700); err != nil { // a directory where the file should be
		t.Fatal(err)
	}
	if _, err := r.service().Migrate(t.Context()); err == nil || errors.Is(err, ErrNoOldService) {
		t.Fatalf("err = %v", err)
	}
	if !has(t, filepath.Join(r.cfg, "f")) || has(t, r.unit("codeflock-worker")) {
		t.Errorf("something changed")
	}
}

func TestServiceRefusesToMoveWhatAPreflightRefuses(t *testing.T) {
	r := newRig(t)
	s := r.service()
	s.Preflight = func(context.Context, Snapshot, Dirs) error { return errors.New("a Run is live") }
	if _, err := s.Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "a Run is live") {
		t.Fatalf("err = %v", err)
	}
	r.unchanged(t)
	if len(r.fleet.Calls()) > 0 && strings.Contains(strings.Join(r.fleet.Calls(), "\n"), " stop ") {
		t.Errorf("the service was stopped: %v", r.fleet.Calls())
	}
}

func TestServiceAndControllerOnTheSameRoot(t *testing.T) {
	r := newRig(t)
	controller := svcunit.Spec{Name: "cf-controller", Label: "com.codefabric.controller"}
	r.installOld(t, controller, true)
	// The Controller runs on the directories the Worker is about to move: refused, nothing changed.
	if _, err := r.service().Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "cf-controller") || !strings.Contains(err.Error(), "cflk controller install-service") {
		t.Fatalf("err = %v", err)
	}
	r.unchanged(t)
	// Stopped, it does not hold the directories: the Worker moves them, and the Controller finds them moved.
	r.fleet.Set("cf-controller.service", true, false)
	if _, err := r.service().Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := r.service()
	s.Old = controller
	s.New = func(_ Snapshot, dirs Dirs) (svcunit.Spec, error) {
		spec := svcunit.Spec{Name: "codeflock-controller", Label: "com.codeflock.controller", Argv: []string{"/opt/cflk/bin/cflk", "controller", "run"}}
		spec.Env = []svcunit.KV{{Key: "CFLK_DATA_DIR", Value: dirs.NewData}}
		return spec, nil
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if has(t, r.unit("cf-controller")) || has(t, r.data) || !has(t, filepath.Join(r.home, ".local", "share", "codeflock", "f")) {
		t.Errorf("the Controller was not moved onto the moved root")
	}
}

func TestServiceLeavesADirectoryTheOwnerChoseWhereItIs(t *testing.T) {
	r := newRig(t)
	mine := t.TempDir()
	r.cfg = mine
	r.installOld(t, oldWorker, true)
	if _, err := r.service().Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !has(t, mine) || !has(t, filepath.Join(r.home, ".local", "share", "codeflock", "f")) {
		t.Errorf("config must stay, data must move")
	}
}

func TestAdoptMovesTheDirectoriesOfAMachineWithNoOldService(t *testing.T) {
	r := newRig(t)
	if err := os.Remove(r.unit("cf-worker")); err != nil {
		t.Fatal(err)
	}
	cfg, data, err := (Mover{}).Adopt(t.Context(), r.env, r.cfg, r.data)
	if err != nil || filepath.Base(cfg) != "codeflock" || filepath.Base(data) != "codeflock" || !has(t, filepath.Join(data, "f")) {
		t.Fatalf("%s %s %v", cfg, data, err)
	}
	// What is not named codefabric is the person's.
	if cfg, data, err = (Mover{}).Adopt(t.Context(), r.env, "/mine/c", "/mine/d"); err != nil || cfg != "/mine/c" || data != "/mine/d" {
		t.Errorf("%s %s %v", cfg, data, err)
	}
}

func TestAdoptRefusesWhileAnOldServiceRunsOnTheDirectories(t *testing.T) {
	r := newRig(t)
	if _, _, err := (Mover{}).Adopt(t.Context(), r.env, r.cfg, r.data); err == nil {
		t.Fatal("moved the directories under a running service")
	}
	if !has(t, filepath.Join(r.data, "f")) {
		t.Errorf("moved")
	}
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // a file of the test
}
