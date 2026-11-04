//go:build linux || darwin

package executor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	"github.com/Autarquico/codefabric/internal/adapters/secrets/secretstest"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// keyringStore is a catalog whose keyring entries live in vault, with the
// secret github.personal set there.
func keyringStore(t *testing.T) (store *secretsfile.Store, vault *secretstest.Vault, catalog string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "worker")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	catalog = filepath.Join(dir, "secrets.json")
	vault = secretstest.New()
	store = secretsfile.New(catalog, clock.NewFake(t0), secretsfile.WithVault(vault))
	if err := store.Set(t.Context(), "github.personal", []byte(ghValue), nil, secrets.BackendKeyring); err != nil {
		t.Fatal(err)
	}
	return store, vault, catalog
}

func keyringFixture(t *testing.T, store secrets.Store) *fixture {
	t.Helper()
	return newFixture(t, func(o *Options) {
		o.Secrets = store
		o.RedactPipeArgv = []string{"/cf", "worker", "redact-pipe"}
	})
}

// prepareFailure runs the first step of a Run that uses github.personal and
// returns its prepare_failed report, after checking that nothing started or
// was delivered and that no value is in any report.
func prepareFailure(t *testing.T, f *fixture) v1.RunReportEntry {
	t.Helper()
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if len(reports) != 2 || reports[0].Kind != v1.ReportPrepareFailed || !reports[1].NeverStarted {
		t.Fatalf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 {
		t.Error("it started")
	}
	if _, err := os.Stat(filepath.Join(f.runDir(k), "secrets")); !os.IsNotExist(err) {
		t.Errorf("a delivery was made: %v", err)
	}
	noValueInReports(t, reports)
	return reports[0]
}

// TestScenario_SecretsKeyring_Keyring: a secret kept in the keyring reaches an
// authorized Run, and the catalog on disk does not hold it.
func TestScenario_SecretsKeyring_Keyring(t *testing.T) {
	store, vault, catalog := keyringStore(t)
	f := keyringFixture(t, store)
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended at its first step")
	}
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
	if v, err := secretjit.Resolver(f.runDir(k))("github.personal"); err != nil || v != ghValue {
		t.Errorf("the delivery resolves to %q, %v", v, err)
	}
	raw, err := os.ReadFile(catalog) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), ghValue) {
		t.Error("the catalog holds the value")
	}
	if vault.Len() != 1 {
		t.Errorf("the keyring holds %d items, want 1", vault.Len())
	}
	noValueIn(t, f.runDir(k))
	noValueInReports(t, f.reports(k))
}

// TestScenario_SecretsKeyring_LlaveroBloqueado: the Worker accepted the Run (a
// locked keyring does not make it stop announcing the secret) and the prepare
// fails with CF_SECRET_UNAVAILABLE and a diagnostic, before anything starts.
func TestScenario_SecretsKeyring_LlaveroBloqueado(t *testing.T) {
	store, vault, _ := keyringStore(t)
	f := keyringFixture(t, store)
	vault.Lock()

	r := prepareFailure(t, f)
	if r.Code != domain.CodeSecretUnavailable {
		t.Errorf("code = %q, want %s", r.Code, domain.CodeSecretUnavailable)
	}
	for _, want := range []string{`"github.personal"`, "keyring", "cannot be reached", "locked"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message = %q, want %q in it", r.Message, want)
		}
	}
	if !slices.Contains(v1.PrepareFailureCodes, r.Code) {
		t.Errorf("%s is not a code the Controller keeps", r.Code)
	}
}

// TestPrepare_AKeyringEntryWhoseValueIsGoneFailsWithTheEntryMissing: the
// diagnostic tells an orphan from a locked keyring.
func TestPrepare_AKeyringEntryWhoseValueIsGoneFailsWithTheEntryMissing(t *testing.T) {
	store, vault, _ := keyringStore(t)
	f := keyringFixture(t, store)
	vault.Lose("github.personal")

	r := prepareFailure(t, f)
	if r.Code != domain.CodeSecretUnavailable || !strings.Contains(r.Message, "entry is missing") || strings.Contains(r.Message, "locked") {
		t.Errorf("report = %+v", r)
	}
}

func TestScopeVerdict_AcceptsKeyringEntries(t *testing.T) {
	catalog := []secrets.Metadata{
		{Ref: "a", Backend: secrets.BackendKeyring},
		{Ref: "b", Backend: secrets.BackendFile},
		{Ref: "c", Backend: "central"},
	}
	if code, msg := scopeVerdict("project_1", []string{"a", "b"}, catalog); code != "" {
		t.Errorf("verdict = %s %q", code, msg)
	}
	if code, _ := scopeVerdict("project_1", []string{"c"}, catalog); code != domain.CodeSecretUnavailable {
		t.Errorf("a backend this Worker does not know: %s", code)
	}
}
