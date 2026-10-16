package daemon

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/state"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

// beating is a daemon that measures a fixed machine and holds the catalog at path.
func beating(t *testing.T, path string) *daemon {
	t.Helper()
	return &daemon{
		epoch: "01J00000000000000000000000", generation: 1, version: "v0.3.1", log: slog.New(slog.DiscardHandler),
		measure: func(string) (sysinfo.Info, error) {
			return sysinfo.Info{Hostname: "box", OS: "linux", Arch: "amd64", CPU: 4, MemoryMB: 4096, DiskMB: 1000}, nil
		},
		secrets: secretsfile.New(path, clock.NewFake(time.Unix(0, 0))),
	}
}

func catalogPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "worker")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "secrets.json")
}

func TestHeartbeat_CarriesTheMetadataOfTheCatalogAndNoValue(t *testing.T) {
	path := catalogPath(t)
	d := beating(t, path)
	sentinel := []byte("Zq8v-sentinel-7Hc2LmP0xR4tW9yB")
	project := "project_01J00000000000000000000000"
	if err := d.secrets.Set(t.Context(), "gh/token", sentinel, []string{project}); err != nil {
		t.Fatal(err)
	}

	req, err := d.heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if req.Generation != 1 || req.Secrets == nil || req.Secrets.Seq != 1 || len(req.Secrets.Refs) != 1 ||
		req.Secrets.Refs[0].Ref != "gh/token" || req.Secrets.Refs[0].Projects[0] != project || req.Health.Status != v1.HealthOK {
		t.Fatalf("heartbeat = %+v", req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range []string{
		string(sentinel), hex.EncodeToString(sentinel), base64.StdEncoding.EncodeToString(sentinel),
		base64.RawURLEncoding.EncodeToString(sentinel), "sentinel",
	} {
		if strings.Contains(string(body), encoded) {
			t.Errorf("the heartbeat carries the value, as %q", encoded)
		}
	}

	next, err := d.heartbeat(t.Context())
	if err != nil || next.Secrets.Seq != 2 {
		t.Errorf("the next heartbeat has seq %d (%v), want 2", next.Secrets.Seq, err)
	}
}

func TestHeartbeat_AnEmptyOrMissingCatalogIsAnEmptyReport(t *testing.T) {
	req, err := beating(t, catalogPath(t)).heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req.Secrets)
	if err != nil || string(body) != `{"seq":1,"refs":[]}` || req.Health.Status != v1.HealthOK {
		t.Errorf("secrets = %s (%v), health %+v", body, err, req.Health)
	}
}

func TestHeartbeat_AnUnreadableCatalogIsReportedEmptyAndDegradesTheWorker(t *testing.T) {
	path := catalogPath(t)
	d := beating(t, path)
	if err := d.secrets.Set(t.Context(), "gh/token", []byte("long-enough-value"), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}

	req, err := d.heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Secrets.Refs) != 0 || req.Health != (v1.Health{Status: v1.HealthDegraded, Message: catalogUnreadable}) {
		t.Errorf("secrets %+v, health %+v", req.Secrets, req.Health)
	}
}

func TestAnswerFence(t *testing.T) {
	retired := func(generation any) error {
		return domain.NewError(domain.CodeStaleLease, "retired", map[string]any{"reason": fenceGenerationRetired, "generation": generation})
	}
	for _, tc := range []struct {
		name      string
		err       error
		laterRun  bool // another run on the same state.db took a generation
		again     bool
		wantGen   uint64
		wantError bool
	}{
		{name: "a retired generation takes one above the Controller's", err: retired(float64(7)), again: true, wantGen: 8},
		{name: "a retired generation superseded on this machine stops", err: retired(float64(7)), laterRun: true, wantError: true},
		{name: "a retired generation with no number is an error", err: retired("seven"), wantError: true, wantGen: 1},
		{name: "an overtaken heartbeat sends the next", err: domain.NewError(domain.CodeStaleLease, "late", map[string]any{"reason": fenceOutOfOrder}), again: true, wantGen: 1},
		{name: "a stale lease of anything else is an error", err: domain.NewError(domain.CodeStaleLease, "x", nil), wantError: true, wantGen: 1},
		{name: "another error is an error", err: domain.NewError(domain.CodeInvalidRequest, "x", nil), wantError: true, wantGen: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := state.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			d := &daemon{stateDB: db, log: slog.New(slog.DiscardHandler)}
			if d.generation, err = db.StartGeneration(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.laterRun {
				if _, err := db.StartGeneration(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			again, err := d.answerFence(t.Context(), tc.err)
			if again != tc.again || (err != nil) != tc.wantError {
				t.Fatalf("again %t, err %v; want %t, error %t", again, err, tc.again, tc.wantError)
			}
			if tc.laterRun && !errors.Is(err, state.ErrSuperseded) {
				t.Errorf("err = %v, want ErrSuperseded", err)
			}
			if !tc.laterRun && d.generation != tc.wantGen {
				t.Errorf("generation %d, want %d", d.generation, tc.wantGen)
			}
		})
	}
}
