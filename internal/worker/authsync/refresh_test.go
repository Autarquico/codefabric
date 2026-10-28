package authsync_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/authsync"
)

// codexAuth is an auth.json as Codex writes it after a refresh, padded so that
// writing it takes more than one write.
func codexAuth(refreshedAt time.Time, who string, n int) []byte {
	b, _ := json.Marshal(map[string]any{
		"tokens":       map[string]string{"access_token": fmt.Sprintf("access-%s-%d", who, n), "refresh_token": fmt.Sprintf("refresh-%s-%d", who, n), "account_id": "acct-user"},
		"last_refresh": refreshedAt.Format(time.RFC3339Nano),
		"pad":          strings.Repeat("x", 8<<10),
	})
	return b
}

// refreshLikeCodex is what Codex does to its auth.json
// (FileAuthStorage::save): open the path with truncate, write, flush; no
// rename, no lock.
func refreshLikeCodex(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600) //nolint:gosec // temporary dir
	if err != nil {
		return err
	}
	half := len(content) / 2
	if _, err := f.Write(content[:half]); err != nil {
		_ = f.Close()
		return err
	}
	runtime.Gosched() // another refresh may truncate here
	if _, err := f.Write(content[half:]); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func TestScenario_RefreshConcurrente(t *testing.T) {
	const refreshes = 150
	home := t.TempDir()
	global := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, codexAuth(epoch, "host", 0), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + home}})
	if err != nil {
		t.Fatal(err)
	}
	adapter := codex.Adapter{}
	b := authsync.New(authsync.Options{Clock: clock.NewFake(epoch.Add(time.Hour))})

	// Two Runs, each with the copy that materialising it gives.
	copies := map[string]string{}
	for _, id := range []string{"a", "b"} {
		dir := t.TempDir()
		res, err := adapter.ApplyConfig(t.Context(), h, harness.NativeSnapshot{Harness: codex.ID}, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: true})
		if err != nil || len(res.AuthCopies) != 1 {
			t.Fatalf("apply %s = %+v, %v", id, res, err)
		}
		copies[id] = filepath.Join(dir, res.AuthCopies[0])
		b.Track(id, adapter, h, []string{copies[id]})
	}

	var (
		fakes, watchers sync.WaitGroup
		stop            = make(chan struct{})
		last            = map[string][]byte{}
		failures        = make(chan string, 16)
	)
	fail := func(format string, args ...any) {
		select {
		case failures <- fmt.Sprintf(format, args...):
		default:
		}
	}
	// A refreshes at epoch+1m.., B at epoch+1m30s..: B's last is the most recent.
	for id, offset := range map[string]time.Duration{"a": 0, "b": 30 * time.Second} {
		path := copies[id]
		final := codexAuth(epoch.Add(time.Minute+offset+refreshes*time.Second), id, refreshes)
		last[id] = final
		fakes.Go(func() {
			for i := 1; i < refreshes; i++ {
				if err := refreshLikeCodex(path, codexAuth(epoch.Add(time.Minute+offset+time.Duration(i)*time.Second), id, i)); err != nil {
					fail("%s refresh: %v", id, err)
					return
				}
			}
			if err := refreshLikeCodex(path, final); err != nil {
				fail("%s refresh: %v", id, err)
			}
		})
	}
	watchers.Go(func() { // the broker, polling as fast as it can
		for {
			select {
			case <-stop:
				return
			default:
				b.Poll()
			}
		}
	})
	watchers.Go(func() { // the auth of the host is JSON at every instant
		for {
			select {
			case <-stop:
				return
			default:
				content, err := os.ReadFile(global) //nolint:gosec // temporary dir
				if err != nil || !json.Valid(content) {
					fail("the auth of the host is not valid JSON (%v, %d bytes)", err, len(content))
					return
				}
			}
		}
	})
	fakes.Wait()
	close(stop)
	watchers.Wait()
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}

	// Once the refreshes end, the next poll brings everything to the latest.
	b.Poll()
	want := last["b"]
	for name, path := range map[string]string{"global": global, "copy a": copies["a"], "copy b": copies["b"]} {
		got, err := os.ReadFile(path) //nolint:gosec // temporary dir
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is not the most recent auth (%d bytes, want %d)", name, len(got), len(want))
		}
	}
}
