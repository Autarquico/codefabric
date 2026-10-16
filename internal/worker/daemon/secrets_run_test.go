package daemon_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func secretRefs(w v1.Worker) []string {
	var refs []string
	for _, m := range w.Secrets {
		refs = append(refs, m.Ref)
	}
	return refs
}

func TestRun_TheHeartbeatPublishesTheCatalogAndNeverAValue(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	catalog := secretsfile.New(paths.WorkerSecretsFile(), clock.Real{})
	const value = "sentinel-0d1c3a7f-value-never-leaves"
	if err := catalog.Set(t.Context(), "gh/token", []byte(value), nil); err != nil {
		t.Fatal(err)
	}
	run(t, paths, &logs{})

	c.eventually("box", "the first secret to be reported", func(w v1.Worker) bool {
		return w.State == domain.WorkerOnline && slices.Equal(secretRefs(w), []string{"gh/token"})
	})
	// Set talks to no one: the next heartbeat carries it.
	if err := catalog.Set(t.Context(), "npm/token", []byte(value+"-2"), nil); err != nil {
		t.Fatal(err)
	}
	w := c.eventually("box", "the second secret to be reported", func(w v1.Worker) bool {
		return slices.Equal(secretRefs(w), []string{"gh/token", "npm/token"})
	})
	shown, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(shown), "sentinel") {
		t.Errorf("the value reached the Controller: %s", shown)
	}
}

func TestRun_ADaemonThatLostItsStateTakesAGenerationAboveTheControllers(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	var last string
	for range 2 {
		r := run(t, paths, &logs{})
		w := c.eventually("box", "a new run to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline && w.Epoch != last })
		last = w.Epoch
		if err := r.stop(); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(paths.WorkerStateFile() + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	out := &logs{}
	r := run(t, paths, out)
	c.eventually("box", "the run with a fresh state to be taken", func(w v1.Worker) bool { return w.State == domain.WorkerOnline && w.Epoch != last })
	if !strings.Contains(out.String(), "taking a new one") {
		t.Errorf("the recovery was not logged:\n%s", out)
	}
	select {
	case <-r.done:
		t.Fatalf("the daemon stopped: %v", r.err)
	default:
	}
}
