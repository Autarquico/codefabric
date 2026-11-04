package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	"github.com/Autarquico/codefabric/internal/adapters/secrets/secretstest"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

const keyringValue = "kr-sentinel-3d9f1c7a-never-in-the-catalog"

// countingVault counts its Gets, and can hold them until ctx ends.
type countingVault struct {
	*secretstest.Vault
	gets atomic.Int32
	hold atomic.Bool
}

func (v *countingVault) Get(ctx context.Context, account string) ([]byte, error) {
	v.gets.Add(1)
	if v.hold.Load() {
		<-ctx.Done()
		return nil, secrets.ErrUnavailable
	}
	return v.Vault.Get(ctx, account)
}

// keyringWorker is a daemon whose catalog keeps gh/token in a fake keyring and
// npm/token in the file, all on one fake clock.
type keyringWorker struct {
	d     *daemon
	store *secretsfile.Store
	vault *countingVault
	clock *clock.Fake
	path  string
}

func newKeyringWorker(t *testing.T) *keyringWorker {
	t.Helper()
	w := &keyringWorker{vault: &countingVault{Vault: secretstest.New()}, clock: clock.NewFake(time.Unix(1_000_000, 0)), path: catalogPath(t)}
	w.store = secretsfile.New(w.path, w.clock, secretsfile.WithVault(w.vault))
	w.d = &daemon{
		epoch: "01J00000000000000000000000", generation: 1, version: "v0.3.1", log: slog.New(slog.DiscardHandler), clock: w.clock,
		measure: func(string) (sysinfo.Info, error) {
			return sysinfo.Info{Hostname: "box", OS: "linux", Arch: "amd64", CPU: 4, MemoryMB: 4096, DiskMB: 1000}, nil
		},
		secrets: w.store,
	}
	w.set(t, "gh/token", secrets.BackendKeyring)
	w.set(t, "npm/token", secrets.BackendFile)
	openSecrets = func() (secrets.Store, error) { return w.store, nil }
	t.Cleanup(func() { openSecrets = defaultOpenSecrets })
	return w
}

// set stores ref, a second later than the last set.
func (w *keyringWorker) set(t *testing.T, ref, backend string) {
	t.Helper()
	w.clock.Advance(time.Second)
	if err := w.store.Set(t.Context(), ref, []byte(keyringValue), nil, backend); err != nil {
		t.Fatal(err)
	}
}

// announced is the refs the next heartbeat carries.
func (w *keyringWorker) announced(t *testing.T) []string {
	t.Helper()
	req, err := w.d.heartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, m := range req.Secrets.Refs {
		refs = append(refs, m.Ref)
	}
	return refs
}

// checked waits for the Check in the background to finish.
func (w *keyringWorker) checked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.d.health.mu.Lock()
		done := !w.d.health.running && !w.d.health.checkedAt.IsZero()
		w.d.health.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the Check did not finish")
}

// recheck lets the cached statuses expire and beats once to start the Check.
func (w *keyringWorker) recheck(t *testing.T) {
	t.Helper()
	w.announced(t)
	w.checked(t)
	w.clock.Advance(secretRecheck)
	w.announced(t)
	w.checked(t)
}

// cf runs `cf worker secrets <args>` against the store of the test.
func cf(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "cf", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("json", false, "")
	worker := &cobra.Command{Use: "worker"}
	worker.AddCommand(newSecretsCmd())
	root.AddCommand(worker)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"worker", "secrets"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// TestScenario_SecretsKeyring_EntradaHuerfana: an entry of the catalog whose
// value left the keyring is marked broken by `list`, and the heartbeat stops
// announcing it; `rm` repairs it, and so does setting it again.
func TestScenario_SecretsKeyring_EntradaHuerfana(t *testing.T) {
	w := newKeyringWorker(t)
	if got := w.announced(t); !slices.Equal(got, []string{"gh/token", "npm/token"}) {
		t.Fatalf("announced = %v", got)
	}
	w.vault.Lose("gh/token")
	w.recheck(t)

	if got := w.announced(t); !slices.Equal(got, []string{"npm/token"}) {
		t.Errorf("announced after the value left the keyring = %v, want only npm/token", got)
	}
	out, err := cf(t, "", "list")
	if err != nil || !hasRow(out, "gh/token", "keyring", "broken") || !hasRow(out, "npm/token", "file", "ok") {
		t.Errorf("list (%v):\n%s", err, out)
	}
	out, err = cf(t, "", "list", "--json")
	var listed struct {
		Secrets []struct {
			Ref    string `json:"ref"`
			Status string `json:"status"`
		} `json:"secrets"`
	}
	if err != nil || json.Unmarshal([]byte(out), &listed) != nil || len(listed.Secrets) != 2 ||
		listed.Secrets[0].Status != "broken" || listed.Secrets[1].Status != "ok" {
		t.Errorf("list --json (%v): %s", err, out)
	}

	if out, err = cf(t, "", "rm", "gh/token"); err != nil || !strings.Contains(out, "Removed secret gh/token") {
		t.Fatalf("rm of a broken entry (%v): %s", err, out)
	}
	if out, _ = cf(t, "", "list"); strings.Contains(out, "gh/token") {
		t.Errorf("the broken entry is still listed:\n%s", out)
	}

	// Setting a broken entry again also repairs it: the new version is announced at once.
	w2 := newKeyringWorker(t)
	w2.vault.Lose("gh/token")
	w2.recheck(t)
	w2.clock.Advance(time.Second)
	if out, err := cf(t, keyringValue, "set", "gh/token", "--backend", "keyring"); err != nil {
		t.Fatalf("set (%v): %s", err, out)
	}
	if got := w2.announced(t); !slices.Equal(got, []string{"gh/token", "npm/token"}) {
		t.Errorf("announced after set again = %v", got)
	}
	w2.checked(t)
	if got := w2.announced(t); !slices.Equal(got, []string{"gh/token", "npm/token"}) {
		t.Errorf("announced once checked = %v", got)
	}
}

// hasRow says whether a row of the table has these cells in order.
func hasRow(table string, cells ...string) bool {
	for _, line := range strings.Split(table, "\n") {
		if fields := strings.Fields(line); len(fields) >= len(cells) && slices.Equal(fields[:len(cells)], cells) {
			return true
		}
	}
	return false
}

func TestHeartbeat_ALockedKeyringKeepsAnnouncingItsSecrets(t *testing.T) {
	w := newKeyringWorker(t)
	w.announced(t)
	w.checked(t)
	w.vault.Lock()
	w.recheck(t)

	if got := w.announced(t); !slices.Equal(got, []string{"gh/token", "npm/token"}) {
		t.Errorf("announced = %v, want both: a locked keyring is not an orphan", got)
	}
	out, err := cf(t, "", "list")
	if err != nil || !hasRow(out, "gh/token", "keyring", "unavailable") {
		t.Errorf("list (%v):\n%s", err, out)
	}
}

func TestHeartbeat_NeverWaitsForTheKeyringAndAsksOnceUntilTheStatusExpires(t *testing.T) {
	w := newKeyringWorker(t)
	w.vault.hold.Store(true) // a keyring that waits for someone to unlock it

	done := make(chan []string, 1)
	go func() { done <- w.announced(t) }()
	select {
	case got := <-done:
		if !slices.Equal(got, []string{"gh/token", "npm/token"}) {
			t.Errorf("announced = %v: what no Check has seen is announced", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the heartbeat waited for the keyring")
	}
	for range 3 {
		w.announced(t)
	}
	if n := w.vault.gets.Load(); n > 1 {
		t.Errorf("the keyring was asked %d times while a Check was running, want 1", n)
	}
}

func TestHeartbeat_TheDoctorNeverReadsTheKeyringOnTheBeat(t *testing.T) {
	w := newKeyringWorker(t)
	w.set(t, claude.OAuthTokenRef, secrets.BackendKeyring)
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	w.d.doctorEvery = time.Minute
	w.d.doctor = doctor.New(doctor.Options{
		Host: h, Adapters: []harness.Adapter{claude.Adapter{}}, DataDir: t.TempDir(), Git: "true", GOOS: "linux",
		Runtime: func(context.Context) error { return nil },
		Disk:    func(string) (sysinfo.Disk, error) { return sysinfo.Disk{FreeBytes: 50, TotalBytes: 100}, nil },
		Secrets: w.store, SecretState: w.d.health.stateOf,
	})
	w.vault.hold.Store(true)

	done := make(chan struct{})
	go func() { w.announced(t); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the heartbeat waited for the keyring through the doctor")
	}
	if n := w.vault.gets.Load(); n > 2 { // the background Check reads each keyring entry once
		t.Errorf("the keyring was asked %d times: the doctor must not Check it", n)
	}
}

func TestHeartbeat_TheCheckIsKeptUntilItExpires(t *testing.T) {
	w := newKeyringWorker(t)
	w.announced(t)
	w.checked(t)
	asked := w.vault.gets.Load()
	for range 3 {
		w.announced(t)
	}
	if got := w.vault.gets.Load(); got != asked {
		t.Errorf("the keyring was asked again within %s: %d -> %d", secretRecheck, asked, got)
	}
	w.recheck(t)
	if got := w.vault.gets.Load(); got != asked+1 {
		t.Errorf("after expiring: %d asks, want %d", got, asked+1)
	}
}

func TestSecretsSet_AnOversizedKeyringValueNamesTheCap(t *testing.T) {
	w := newKeyringWorker(t)
	out, err := cf(t, strings.Repeat("x", 2049), "set", "big/token", "--backend", "keyring")
	if err == nil || !strings.Contains(err.Error(), "2048") {
		t.Fatalf("set = %v, %s", err, out)
	}
	if _, gerr := w.store.Get(t.Context(), "big/token"); !errors.Is(gerr, secrets.ErrNotFound) {
		t.Errorf("the oversized secret was stored: %v", gerr)
	}
}

func TestSecretsSet_BackendKeyringKeepsTheValueOutOfTheCatalog(t *testing.T) {
	w := newKeyringWorker(t)
	if out, err := cf(t, keyringValue, "set", "other/token", "--backend", "keyring", "--json"); err != nil || strings.Contains(out, keyringValue) {
		t.Fatalf("set (%v): %s", err, out)
	} else if !strings.Contains(out, `"backend":"keyring"`) {
		t.Errorf("set --json = %s", out)
	}
	// npm/token is file-backed and holds keyringValue by design: another value there.
	if err := w.store.Set(t.Context(), "npm/token", []byte("file-held-value-0000"), nil, secrets.BackendFile); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(w.path) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	secretstest.RequireValueOutOfCatalog(t, raw, "other/token", []byte(keyringValue))
	if _, err := cf(t, keyringValue, "set", "x/token", "--backend", "central"); domainCode(err) != domain.CodeInvalidRequest {
		t.Errorf("an unknown backend: %v", err)
	}
	if _, err := cf(t, keyringValue, "set", "y/token"); err != nil {
		t.Fatal(err)
	}
	if list, _ := w.store.List(t.Context()); !slices.ContainsFunc(list, func(m secrets.Metadata) bool { return m.Ref == "y/token" && m.Backend == secrets.BackendFile }) {
		t.Errorf("the default backend is not file: %+v", list)
	}
}

func TestSecretsSet_ALockedKeyringIsReportedAsUnavailableWithoutValues(t *testing.T) {
	w := newKeyringWorker(t)
	w.vault.Lock()
	out, err := cf(t, keyringValue, "set", "other/token", "--backend", "keyring")
	if domainCode(err) != domain.CodeSecretUnavailable {
		t.Fatalf("err = %v, want %s", err, domain.CodeSecretUnavailable)
	}
	if text := out + err.Error(); strings.Contains(text, keyringValue) || strings.Contains(text, "other/token") || !strings.Contains(err.Error(), "cannot be reached") {
		t.Errorf("message = %q", text)
	}
}

func TestSecretsError_UnavailableIsCodedWithADiagnostic(t *testing.T) {
	err := secretsError(errors.Join(secrets.ErrUnavailable, errors.New("detail")))
	if domainCode(err) != domain.CodeSecretUnavailable || !strings.Contains(err.Error(), keyringHint()) {
		t.Errorf("err = %v", err)
	}
}

func domainCode(err error) domain.Code {
	var coded *domain.Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func TestHeartbeat_TheStatusOfARefIsNotTheStatusOfItsNextVersion(t *testing.T) {
	h := &secretHealth{}
	at := time.Unix(10, 0)
	m := secrets.Metadata{Ref: "a", Backend: secrets.BackendKeyring, UpdatedAt: at}
	if !h.due([]secrets.Metadata{m}, at) {
		t.Fatal("a keyring entry no Check has seen is due")
	}
	h.record([]secrets.Metadata{m}, []secrets.Status{{Ref: "a", State: secrets.StateBroken}}, nil, at)
	if st, ok := h.stateOf(m); !ok || st != secrets.StateBroken {
		t.Errorf("state = %v %v", st, ok)
	}
	m.UpdatedAt = at.Add(time.Second)
	if _, ok := h.stateOf(m); ok {
		t.Error("a new version of the secret inherited the state of the old one")
	}
	if !h.due([]secrets.Metadata{m}, at) {
		t.Error("a new version is due for a Check")
	}
	if h.due([]secrets.Metadata{m}, at) {
		t.Error("two Checks at once")
	}
	h.record([]secrets.Metadata{m}, nil, errors.New("boom"), at)
	if !h.due([]secrets.Metadata{{Ref: "f", Backend: secrets.BackendFile}}, at.Add(time.Hour)) {
		t.Error("a stale Check is due even for a catalog of files: retired keyring items are swept by it")
	}
}

func TestSecretHealthChecksPeriodicallyEvenWithNoKeyringEntry(t *testing.T) {
	var h secretHealth
	now := time.Unix(1_000_000, 0)
	if !h.due(nil, now) {
		t.Fatal("the first beat does not check")
	}
	h.record(nil, nil, nil, now)
	if h.due(nil, now.Add(secretRecheck-time.Second)) {
		t.Error("checked again before the recheck")
	}
	if !h.due([]secrets.Metadata{{Ref: "f", Backend: secrets.BackendFile}}, now.Add(secretRecheck)) {
		t.Error("retired items of the keyring are never retried once no keyring entry is left")
	}
}
