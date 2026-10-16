package file

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	projectA = "project_01J00000000000000000000000"
	projectB = "project_01J00000000000000000000001"
)

// sentinel is a value no message, listing or log may contain, in any encoding
// a test looks for.
var sentinel = []byte("s3ntinel-VALUE-do-not-leak")

func newStore(t *testing.T) (*Store, *clock.Fake, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker", "secrets.json")
	c := clock.NewFake(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	return New(path, c), c, path
}

func mustSet(t *testing.T, s *Store, ref string, v []byte, projects ...string) {
	t.Helper()
	if err := s.Set(context.Background(), ref, v, projects); err != nil {
		t.Fatalf("Set(%q): %v", ref, err)
	}
}

func readCatalog(t *testing.T, path string) catalog {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is inside t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStore_SetThenGetAndListGiveTheValueAndOnlyMetadata(t *testing.T) {
	s, c, path := newStore(t)
	ctx := context.Background()
	mustSet(t, s, "b/token", sentinel, projectB, projectA, projectB)
	created := c.Now()
	c.Advance(time.Minute)
	mustSet(t, s, "a.key", []byte("12345678"))

	got, err := s.Get(ctx, "b/token")
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	metas, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []secrets.Metadata{
		{Ref: "a.key", Backend: secrets.BackendFile, CreatedAt: c.Now(), UpdatedAt: c.Now()},
		{Ref: "b/token", Backend: secrets.BackendFile, Projects: []string{projectA, projectB}, CreatedAt: created, UpdatedAt: created},
	}
	if fmt.Sprint(metas) != fmt.Sprint(want) {
		t.Fatalf("List =\n%v\nwant\n%v", metas, want)
	}

	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != filePerm {
		t.Errorf("catalog mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != dirPerm {
		t.Errorf("directory mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
}

func TestStore_SetReplacesAValueKeepingWhenItWasCreated(t *testing.T) {
	s, c, path := newStore(t)
	mustSet(t, s, "tok", []byte("first-value"), projectA)
	first := readCatalog(t, path)
	created := c.Now()
	c.Advance(time.Hour)
	mustSet(t, s, "tok", []byte("second-value"))

	got, err := s.Get(context.Background(), "tok")
	if err != nil || string(got) != "second-value" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	metas, _ := s.List(context.Background())
	if len(metas) != 1 || !metas[0].CreatedAt.Equal(created) || !metas[0].UpdatedAt.Equal(c.Now()) || metas[0].Projects != nil {
		t.Fatalf("List = %+v", metas)
	}
	second := readCatalog(t, path)
	if second.CatalogID == "" || second.CatalogID != first.CatalogID || second.Generation != first.Generation+1 {
		t.Errorf("catalog id %q→%q, generation %d→%d", first.CatalogID, second.CatalogID, first.Generation, second.Generation)
	}
}

func TestStore_WithoutACatalogThereAreNoSecrets(t *testing.T) {
	s, _, path := newStore(t)
	ctx := context.Background()
	if metas, err := s.List(ctx); err != nil || len(metas) != 0 {
		t.Fatalf("List = %v, %v", metas, err)
	}
	if _, err := s.Get(ctx, "tok"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "tok"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Delete = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed Delete wrote the catalog: %v", err)
	}
}

func TestStore_DeleteForgetsTheSecret(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	mustSet(t, s, "keep", []byte("keep-value"))
	mustSet(t, s, "drop", sentinel)
	if err := s.Delete(ctx, "drop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "drop"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
	if metas, _ := s.List(ctx); len(metas) != 1 || metas[0].Ref != "keep" {
		t.Fatalf("List = %v", metas)
	}
	if err := s.Delete(ctx, "drop"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("second Delete = %v", err)
	}
}

func TestStore_SetRefusesWhatIsNotValidAndWritesNothing(t *testing.T) {
	ninth := make([]string, domain.MaxSecretProjects+1)
	for i := range ninth {
		ninth[i] = fmt.Sprintf("project_01J0000000000000000000000%d", i)
	}
	for name, tc := range map[string]struct {
		ref      string
		value    []byte
		projects []string
	}{
		"value below 8 bytes": {"tok", []byte("1234567"), nil},
		"empty value":         {"tok", nil, nil},
		"value above 64 KiB":  {"tok", bytes.Repeat([]byte{'x'}, domain.MaxSecretValueLen+1), nil},
		"ref with a space":    {"bad ref", sentinel, nil},
		"empty ref":           {"", sentinel, nil},
		"project not an ID":   {"tok", sentinel, []string{"my-project"}},
		"ID of another kind":  {"tok", sentinel, []string{"run_01J00000000000000000000000"}},
		"too many Projects":   {"tok", sentinel, ninth},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, path := newStore(t)
			err := s.Set(context.Background(), tc.ref, tc.value, tc.projects)
			if !errors.Is(err, domain.ErrInvalidSecret) {
				t.Fatalf("Set = %v, want ErrInvalidSecret", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused Set wrote the catalog: %v", err)
			}
		})
	}
}

func TestStore_ValuesOfEightBytesUpTo64KiBAreKept(t *testing.T) {
	s, _, _ := newStore(t)
	for _, n := range []int{domain.MinSecretValueLen, domain.MaxSecretValueLen} {
		v := bytes.Repeat([]byte{0xff}, n)
		mustSet(t, s, "tok"+strconv.Itoa(n), v)
		if got, err := s.Get(context.Background(), "tok"+strconv.Itoa(n)); err != nil || !bytes.Equal(got, v) {
			t.Fatalf("%d bytes: Get gave %d bytes, %v", n, len(got), err)
		}
	}
}

func TestStore_ACatalogHoldsAtMost256Refs(t *testing.T) {
	s, _, _ := newStore(t)
	for i := range domain.MaxWorkerSecrets {
		mustSet(t, s, "tok"+strconv.Itoa(i), sentinel)
	}
	if err := s.Set(context.Background(), "one-more", sentinel, nil); !errors.Is(err, secrets.ErrFull) {
		t.Fatalf("Set beyond the limit = %v, want ErrFull", err)
	}
	mustSet(t, s, "tok0", []byte("replaced-value")) // replacing adds no ref
}

func TestStore_GetReturnsACopy(t *testing.T) {
	s, _, _ := newStore(t)
	mustSet(t, s, "tok", sentinel)
	got, _ := s.Get(context.Background(), "tok")
	got[0] = 'X'
	if again, _ := s.Get(context.Background(), "tok"); !bytes.Equal(again, sentinel) {
		t.Fatalf("changing what Get returned changed the secret: %q", again)
	}
}

func TestStore_RefusesACatalogOthersCouldReadOrReplace(t *testing.T) {
	ctx := context.Background()
	for name, spoil := range map[string]func(t *testing.T, path string){
		"catalog readable by the group": func(t *testing.T, path string) { chmod(t, path, 0o640) },
		"catalog readable by anyone":    func(t *testing.T, path string) { chmod(t, path, 0o604) },
		"directory open to the group":   func(t *testing.T, path string) { chmod(t, filepath.Dir(path), 0o750) },
		"directory open to anyone":      func(t *testing.T, path string) { chmod(t, filepath.Dir(path), 0o701) },
		"catalog is a symlink": func(t *testing.T, path string) {
			target := filepath.Join(filepath.Dir(path), "real.json")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, path := newStore(t)
			mustSet(t, s, "tok", sentinel)
			spoil(t, path)
			if _, err := s.List(ctx); !errors.Is(err, secrets.ErrUnsafe) {
				t.Errorf("List = %v, want ErrUnsafe", err)
			}
			if _, err := s.Get(ctx, "tok"); !errors.Is(err, secrets.ErrUnsafe) {
				t.Errorf("Get = %v, want ErrUnsafe", err)
			}
			if err := s.Set(ctx, "other", sentinel, nil); !errors.Is(err, secrets.ErrUnsafe) {
				t.Errorf("Set = %v, want ErrUnsafe", err)
			}
		})
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func writeCatalog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		t.Fatal(err)
	}
}

func TestStore_ACatalogThatIsNotValidIsRefusedWithoutQuotingIt(t *testing.T) {
	leak := string(sentinel)
	ok := `"created_at":"2026-10-16T12:00:00Z","updated_at":"2026-10-16T12:00:00Z"`
	for name, content := range map[string]string{
		"bare token":           `{"schema":1,"secrets":` + leak + `}`,
		"truncated":            `{"schema":1,"secrets":{"tok":{"backend":"file","value":"` + leak,
		"value not base64":     `{"schema":1,"secrets":{"tok":{"backend":"file","value":"` + leak + `!",` + ok + `}}}`,
		"wrong type":           `{"schema":"` + leak + `"}`,
		"unknown schema":       `{"schema":2,"secrets":{}}`,
		"value too short":      `{"schema":1,"secrets":{"tok":{"backend":"file","value":"YWJj",` + ok + `}}}`,
		"no backend":           `{"schema":1,"secrets":{"tok":{"value":"czNudGluZWwtVkFMVUU=",` + ok + `}}}`,
		"invalid ref":          `{"schema":1,"secrets":{"a b":{"backend":"file","value":"czNudGluZWwtVkFMVUU=",` + ok + `}}}`,
		"keyring with a value": `{"schema":1,"secrets":{"tok":{"backend":"keyring","value":"czNudGluZWwtVkFMVUU=",` + ok + `}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, path := newStore(t)
			writeCatalog(t, path, content)
			_, err := s.List(context.Background())
			if !errors.Is(err, secrets.ErrCorrupt) {
				t.Fatalf("List = %v, want ErrCorrupt", err)
			}
			assertNoValue(t, err.Error())
			if err := s.Set(context.Background(), "other", []byte("other-value"), nil); !errors.Is(err, secrets.ErrCorrupt) {
				t.Errorf("Set over it = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestStore_ASyntaxErrorSaysWhereItIs(t *testing.T) {
	s, _, path := newStore(t)
	writeCatalog(t, path, `{"schema":1,"secrets":`+string(sentinel)+`}`)
	if _, err := s.List(context.Background()); err == nil || !strings.Contains(err.Error(), "(offset 23)") {
		t.Fatalf("List = %v, want the offset", err)
	}
}

func TestStore_ListsASecretOfAnotherBackendAndCannotReadIt(t *testing.T) {
	s, _, path := newStore(t)
	writeCatalog(t, path, `{"schema":1,"catalog_id":"x","generation":3,"secrets":{"kr":{"backend":"keyring","created_at":"2026-10-16T12:00:00Z","updated_at":"2026-10-16T12:00:00Z"}}}`)
	metas, err := s.List(context.Background())
	if err != nil || len(metas) != 1 || metas[0].Backend != "keyring" {
		t.Fatalf("List = %v, %v", metas, err)
	}
	if _, err := s.Get(context.Background(), "kr"); !errors.Is(err, secrets.ErrUnsupportedBackend) {
		t.Fatalf("Get = %v, want ErrUnsupportedBackend", err)
	}
	mustSet(t, s, "file-one", sentinel) // a Set keeps the entry of the other backend
	if metas, _ := s.List(context.Background()); len(metas) != 2 {
		t.Fatalf("List after Set = %v", metas)
	}
}

func TestStore_NoValueReachesAnErrorAListingOrALog(t *testing.T) {
	s, _, path := newStore(t)
	ctx := context.Background()
	mustSet(t, s, "tok", sentinel, projectA)

	var said []string
	errs := []error{
		s.Set(ctx, "bad ref", sentinel, nil),
		s.Set(ctx, "tok", append(slices.Clone(sentinel), bytes.Repeat([]byte{'x'}, domain.MaxSecretValueLen)...), nil),
		s.Set(ctx, "tok", sentinel, []string{"not-a-project"}),
		s.Delete(ctx, "missing"),
	}
	_, getErr := s.Get(ctx, "missing")
	errs = append(errs, getErr)
	chmod(t, path, 0o644)
	_, unsafeErr := s.Get(ctx, "tok")
	errs = append(errs, unsafeErr)
	chmod(t, path, filePerm)
	for _, err := range errs {
		if err == nil {
			t.Fatal("a failing call returned no error")
		}
		said = append(said, err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
	}

	metas, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
		said = append(said, fmt.Sprintf(verb, metas), fmt.Sprintf(verb, c), fmt.Sprintf(verb, c.Secrets["tok"]))
	}

	var log bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&log, nil), slog.NewJSONHandler(&log, nil)} {
		slog.New(h).Info("secrets", "list", metas, "catalog", c, "entry", c.Secrets["tok"], "value", c.Secrets["tok"].Value)
	}
	said = append(said, log.String())

	listed, err := json.Marshal(metas)
	if err != nil {
		t.Fatal(err)
	}
	said = append(said, string(listed))

	for _, text := range said {
		assertNoValue(t, text)
	}
}

// assertNoValue fails if text holds the sentinel, as text, hex or base64.
func assertNoValue(t *testing.T, text string) {
	t.Helper()
	for _, form := range []string{
		string(sentinel), "s3ntinel", fmt.Sprintf("%x", sentinel), fmt.Sprintf("%X", sentinel),
		base64.StdEncoding.EncodeToString(sentinel[:12]), // a prefix of the base64 of any value that starts with it
	} {
		if strings.Contains(text, form) {
			t.Fatalf("the value leaked as %q into:\n%s", form, text)
		}
	}
}

func TestStore_ConcurrentSetsInOneProcessLoseNothing(t *testing.T) {
	s, _, path := newStore(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			// Two Stores, two descriptors: flock serializes them as it does processes.
			other := New(path, clock.Real{})
			if err := other.Set(context.Background(), "tok"+strconv.Itoa(i), sentinel, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if metas, err := s.List(context.Background()); err != nil || len(metas) != 16 {
		t.Fatalf("List has %d secrets, %v; want 16", len(metas), err)
	}
	if c := readCatalog(t, path); c.Generation != 16 {
		t.Errorf("generation = %d, want 16", c.Generation)
	}
}

const helperEnv = "CF_SECRETS_FILE_HELPER"

// TestHelperProcess is a child of TestStore_ConcurrentSetsAcrossProcessesLoseNothing.
func TestHelperProcess(t *testing.T) {
	spec := os.Getenv(helperEnv)
	if spec == "" {
		t.Skip("run as a child process only")
	}
	path, prefix, _ := strings.Cut(spec, "|")
	s := New(path, clock.Real{})
	for i := range 10 {
		if err := s.Set(context.Background(), prefix+strconv.Itoa(i), sentinel, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStore_ConcurrentSetsAcrossProcessesLoseNothing(t *testing.T) {
	s, _, path := newStore(t)
	const children = 4
	cmds := make([]*exec.Cmd, children)
	for i := range cmds {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), helperEnv+"="+path+"|p"+strconv.Itoa(i)+"-")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(out.String())
			}
		})
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child: %v", err)
		}
	}
	if metas, err := s.List(context.Background()); err != nil || len(metas) != children*10 {
		t.Fatalf("List has %d secrets, %v; want %d", len(metas), err, children*10)
	}
}

func TestStore_ASetWaitsForTheLockOnlyAsLongAsItsContext(t *testing.T) {
	s, _, path := newStore(t)
	mustSet(t, s, "tok", sentinel)
	holder, err := os.OpenFile(path+".lock", os.O_RDWR, 0) //nolint:gosec // path is inside t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil { //nolint:gosec // a descriptor fits an int
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Set(ctx, "other", sentinel, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Set under a held lock = %v, want DeadlineExceeded", err)
	}
	if _, err := s.Get(context.Background(), "tok"); err != nil {
		t.Fatalf("a reader waited for the writers' lock: %v", err)
	}
}
