package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

func TestServiceRefusesWhileAnotherMigrationHoldsTheLock(t *testing.T) {
	noOverrides(t)
	r := newRig(t)
	release, err := lockMigration(r.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.service().Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "another cflk") {
		t.Fatalf("err = %v", err)
	}
	r.unchanged(t)
	// The adoption of the directories takes the same lock, whatever the spelling of the path.
	link := filepath.Join(r.home, "link")
	if err := os.Symlink(r.data, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.unit("cf-worker")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Mover{}).Adopt(t.Context(), r.env, r.cfg, r.data, nil); err == nil || !strings.Contains(err.Error(), "another cflk") {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := lockMigration(link); err == nil {
		t.Error("a link to the data directory found another lock")
	}
	release()
	if _, _, err := (Mover{}).Adopt(t.Context(), r.env, r.cfg, r.data, nil); err != nil {
		t.Fatal(err)
	}
}

func TestServiceChecksForRunsAgainOnceTheOldOneIsStopped(t *testing.T) {
	r := newRig(t)
	s := r.service()
	asked := 0
	s.LiveRuns = func(context.Context, string, string) error {
		if asked++; asked == 2 {
			return errors.New("1 Runs are live on this Worker")
		}
		return nil
	}
	if _, err := s.Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "1 Runs are live") {
		t.Fatalf("err = %v", err)
	}
	if asked != 2 || !strings.Contains(strings.Join(r.fleet.Calls(), "\n"), " stop cf-worker") {
		t.Errorf("asked %d times; calls %v", asked, r.fleet.Calls())
	}
	r.unchanged(t)
}

// A pair that this run did not move is not to be put back when a later one fails.
func TestServiceUndoesOnlyThePairsItMoved(t *testing.T) {
	r := newRig(t)
	// The config directory was moved before, by hand: only the new one exists.
	newCfg := filepath.Join(r.home, ".config", "codeflock")
	if err := os.Rename(r.cfg, newCfg); err != nil {
		t.Fatal(err)
	}
	s := r.service()
	boom := errors.New("boom")
	s.Mover.Rename = func(oldPath, newPath string) error {
		if strings.HasSuffix(newPath, filepath.Join("share", "codeflock")) {
			return boom
		}
		return renameNoReplace(oldPath, newPath)
	}
	if _, err := s.Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if !has(t, filepath.Join(newCfg, "f")) || has(t, r.cfg) {
		t.Errorf("a directory this run never moved was moved back")
	}
	if !has(t, filepath.Join(r.data, "f")) || !r.fleet.Active("cf-worker.service") {
		t.Errorf("the rest is not as it was")
	}
}

func TestServiceRecordsWhatItMovedForARunThatResumes(t *testing.T) {
	r := newRig(t)
	s := r.service()
	r.fleet.Fail("systemctl --user start codeflock-worker", errors.New("boom"))
	r.fleet.Fail("systemctl --user start cf-worker", errors.New("boom")) // cannot be put back: the progress stays
	if _, err := s.Migrate(t.Context()); err == nil {
		t.Fatal("no error")
	}
	newData := filepath.Join(r.home, ".local", "share", "codeflock")
	saved, err := (progressFile{dirs: Dirs{OldData: r.data, NewData: newData}}).read()
	if err != nil || saved == nil || len(saved.Moved) != 2 || saved.Phase != "" {
		t.Fatalf("%+v %v", saved, err)
	}
}

func TestServiceRefusesASuccessorThatIsInstalledAlready(t *testing.T) {
	r := newRig(t)
	r.installOld(t, newWorker, true) // a unit of the new name, installed and running, next to the old one
	_, err := r.service().Migrate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "codeflock-worker is installed already") || !strings.Contains(err.Error(), "surplus") {
		t.Fatalf("err = %v", err)
	}
	if !has(t, r.unit("codeflock-worker")) || !r.fleet.Active("codeflock-worker.service") || !r.fleet.Active("cf-worker.service") || !has(t, filepath.Join(r.cfg, "f")) {
		t.Errorf("something changed")
	}
	if strings.Contains(strings.Join(r.fleet.Calls(), "\n"), " stop ") {
		t.Errorf("a service was stopped: %v", r.fleet.Calls())
	}
}

// Cut while tidying: the new services answered, so retrying only finishes the tidying.
func TestServiceCutWhileTidyingOnlyFinishesTheTidying(t *testing.T) {
	r := newRig(t)
	r.installOld(t, oldController, true)
	newData := filepath.Join(r.home, ".local", "share", "codeflock")
	r.fleet.Fail("systemctl --user disable --no-reload cf-controller", errors.New("boom"))
	s := r.service()
	s.Data = newData
	res, err := s.Migrate(t.Context())
	if err != nil || len(res.Warnings) != 1 {
		t.Fatalf("%v %v", res.Warnings, err)
	}
	if has(t, r.unit("cf-worker")) || !has(t, r.unit("cf-controller")) {
		t.Fatal("the set-up of the test is not that of a cut")
	}
	saved, err := (progressFile{dirs: Dirs{OldData: r.data, NewData: newData}}).read()
	if err != nil || saved.Phase != phaseSettled {
		t.Fatalf("the progress does not say it settled: %+v %v", saved, err)
	}
	// Retried from the Worker, whose old unit is gone: the normal path of install-service.
	r.fleet.Fail("systemctl --user disable --no-reload cf-controller", nil)
	n := len(r.fleet.Calls())
	res, err = s.Migrate(t.Context())
	if err != nil || len(res.Warnings) != 0 || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	for _, call := range r.fleet.Calls()[n:] {
		if strings.Contains(call, " stop codeflock") || strings.Contains(call, " start ") || strings.Contains(call, " restart ") {
			t.Errorf("something that settled was touched: %s", call)
		}
	}
	if has(t, r.unit("cf-controller")) || !r.fleet.Active("codeflock-worker.service") || !r.fleet.Active("codeflock-controller.service") || has(t, filepath.Join(newData, progressName)) {
		t.Errorf("not finished")
	}
	// And with nothing left it is the ordinary install-service again.
	if _, err := s.Migrate(t.Context()); !errors.Is(err, ErrNoOldService) {
		t.Errorf("err = %v", err)
	}
}

func TestServiceRetriedThroughASurvivingOldPeerOnlyTidiesAndKeepsWhatWasStopped(t *testing.T) {
	r := newRig(t)
	r.installOld(t, oldController, false) // it was stopped: its successor must stay stopped
	r.fleet.Fail("systemctl --user disable --no-reload cf-controller", errors.New("boom"))
	if _, err := r.service().Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.fleet.Fail("systemctl --user disable --no-reload cf-controller", nil)
	c := r.service()
	c.Old = oldController
	c.New = func(Snapshot, Dirs) (svcunit.Spec, error) { return svcunit.Spec{}, errors.New("not reached") }
	c.LiveRuns = func(context.Context, string, string) error { return errors.New("not asked") }
	if _, err := c.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if has(t, r.unit("cf-controller")) || r.fleet.Active("codeflock-controller.service") || !r.fleet.Active("codeflock-worker.service") {
		t.Errorf("controller active %v", r.fleet.Active("codeflock-controller.service"))
	}
}

func TestServiceSettledNeedsASteadyRunningNotARestarting(t *testing.T) {
	for name, hook := range map[string]func(*rig){
		"restarted by itself":   func(r *rig) { r.fleet.CountRestart("codeflock-worker.service") },
		"on its way to restart": func(r *rig) { r.fleet.SetSubState("codeflock-worker.service", "auto-restart") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			s := r.service()
			s.Settle = func(context.Context) error { hook(r); return nil }
			if _, err := s.Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "codeflock-worker") {
				t.Fatalf("err = %v", err)
			}
			r.unchanged(t)
		})
	}
}

func TestServiceReadsTheDirectoriesOfAUnitCFLKOverCF(t *testing.T) {
	dirs, err := DirsOf(map[string]string{
		"CF_CONFIG_DIR": "/a/codefabric", "CF_DATA_DIR": "/b/codefabric", "CFLK_DATA_DIR": "/c/codefabric",
	})
	if err != nil || dirs.OldConfig != "/a/codefabric" || dirs.OldData != "/c/codefabric" {
		t.Errorf("%+v %v", dirs, err)
	}
	got := CarriedEnv(map[string]string{
		"CF_CONFIG_DIR": "x", "CFLK_DATA_DIR": "y", "CF_FOO": "old", "CFLK_FOO": "new", "CF_BAR": "bar", "PATH": "/p", "TMUX_TMPDIR": "/t",
	}, "PATH")
	var keys []string
	for _, kv := range got {
		keys = append(keys, kv.Key+"="+kv.Value)
	}
	if strings.Join(keys, " ") != "CFLK_FOO=new CF_BAR=bar TMUX_TMPDIR=/t" {
		t.Errorf("carried %v", keys)
	}
}

func TestSuccessorOfASiblingHasNoRepeatedKeyAndTheDirectoriesOfItsCFLKUnit(t *testing.T) {
	s := Service{Env: svcunit.Env{Home: "/Users/ana", GOOS: "linux"}, Binary: "/bin/sh"}
	m := member{
		old:  svcunit.Spec{Name: "cf-controller", Label: "com.codefabric.controller"},
		kind: "controller",
		snap: Snapshot{Argv: []string{"/x/cf", "controller", "run"}, Env: map[string]string{
			"CF_DATA_DIR": "/stale/codefabric", "CFLK_DATA_DIR": "/d/codefabric", "CF_FOO": "old", "CFLK_FOO": "new",
		}},
	}
	spec, err := s.successor(1, m, Dirs{OldConfig: "/c", NewConfig: "/c", OldData: "/d/codefabric", NewData: "/d/codeflock"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, kv := range spec.Env {
		if _, twice := seen[kv.Key]; twice {
			t.Errorf("%s twice", kv.Key)
		}
		seen[kv.Key] = kv.Value
	}
	if seen["CFLK_DATA_DIR"] != "/d/codeflock" || seen["CFLK_FOO"] != "new" || seen["CF_FOO"] != "" || seen["CF_DATA_DIR"] != "" {
		t.Errorf("%v", seen)
	}
}

func TestServiceFindsAnotherServiceOnTheSameRootThroughALink(t *testing.T) {
	r := newRig(t)
	link := filepath.Join(r.home, "data-link")
	if err := os.Symlink(r.data, link); err != nil {
		t.Fatal(err)
	}
	data := r.data
	r.data = link
	r.installOld(t, oldController, true)
	r.data = data
	if _, err := r.service().Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if has(t, r.unit("cf-controller")) || !r.fleet.Active("codeflock-controller.service") {
		t.Errorf("the Controller on the same root, by a link, was left behind")
	}
	got, _, _ := svcunit.InstalledEnv(r.env, svcunit.Spec{Name: "codeflock-controller", Label: "com.codeflock.controller"})
	if got["CFLK_DATA_DIR"] != filepath.Join(r.home, ".local", "share", "codeflock") {
		t.Errorf("%v", got)
	}
}

func TestServiceRefusesAUnitWhoseNameIsNotPlain(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.unit("x")), "cf-worker$(x).service"), []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service().Migrate(t.Context()); err == nil || !strings.Contains(err.Error(), "characters") {
		t.Fatalf("err = %v", err)
	}
	r.unchanged(t)
}
