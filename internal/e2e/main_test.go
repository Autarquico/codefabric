//go:build e2e && (linux || darwin)

package e2e

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reapTimeout bounds each reaping TestMain does, before and after the tests.
const reapTimeout = time.Minute

// TestMain reaps what earlier invocations that died left, opens the root of
// this one, gets cf, runs the tests and reaps the root again: a test that
// panicked may have run no cleanup. A root that cannot be reaped fails the
// invocation.
func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) (code int) {
	parent, err := rootParent()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	reaped, err := reapAbandoned(ctx, parent)
	cancel()
	for _, root := range reaped {
		fmt.Fprintf(os.Stderr, "e2e: reaped the abandoned root %s\n", root)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: reaping abandoned roots: %v\n", err)
	}

	own, err := self(os.Getenv(envKeep) == "1")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if inv, err = openInvocation(parent, own); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: the root of this invocation: %v\n", err)
		return 1
	}
	code = 1
	defer func() {
		if !finish(inv, code) {
			code = cmp.Or(code, 1)
		}
	}()

	bin := filepath.Join(inv.dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return code
	}
	if binary, race, err = resolveBinary(bin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return code
	}
	code = m.Run()
	return code
}

// finish reaps the root of the invocation, copies its logs out if it failed and
// CF_E2E_ARTIFACTS says where, and removes it unless CF_E2E_KEEP=1. A root it
// could not reap is a failure: its logs are copied and it is kept, as the
// evidence and for the reaper of the next invocation. It says whether it reaped.
func finish(inv *invocation, code int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	reapErr := reapRoot(ctx, inv.dir)
	if reapErr != nil {
		fmt.Fprintf(os.Stderr, "e2e: reaping %s: %v\n", inv.dir, reapErr)
	}
	_ = inv.close()
	if dest := os.Getenv(envArtifacts); dest != "" && (code != 0 || reapErr != nil) {
		if err := copyLogs(inv.dir, filepath.Join(dest, filepath.Base(inv.dir))); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: copying the logs: %v\n", err)
		}
	}
	if inv.keep || reapErr != nil {
		fmt.Fprintf(os.Stderr, "e2e: kept %s\n", inv.dir)
		return reapErr == nil
	}
	if err := os.RemoveAll(inv.dir); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: removing %s: %v\n", inv.dir, err)
	}
	return true
}

// copyLogs copies the manifest, the owner and every logs/ directory of root
// into dest, with every invite of the cluster redacted: artifacts may be
// public, and an invite is a credential, even of a Controller that is gone.
func copyLogs(root, dest string) error {
	secrets := invites(root)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // what went meanwhile is not copied
		}
		rel, _ := filepath.Rel(root, path)
		wanted := rel == manifestFile || rel == ownerFile || filepath.Base(filepath.Dir(path)) == "logs"
		if d.IsDir() || !d.Type().IsRegular() || !wanted {
			return nil
		}
		return copyRedacted(path, filepath.Join(dest, rel), secrets)
	})
}

// invites are the arguments that follow join or connect in the argv of the
// manifest of root: what `cf controller token create` printed.
func invites(root string) []string {
	f, err := os.Open(filepath.Join(root, manifestFile)) //nolint:gosec // a root of this suite
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scan.Scan() {
		var e entry
		if json.Unmarshal(scan.Bytes(), &e) != nil {
			continue
		}
		for i, arg := range e.Argv[:max(len(e.Argv)-1, 0)] {
			if arg == "join" || arg == "connect" {
				out = append(out, e.Argv[i+1])
			}
		}
	}
	return out
}

// copyRedacted copies from to to, with every secret in it replaced.
func copyRedacted(from, to string, secrets []string) error {
	raw, err := os.ReadFile(from) //nolint:gosec // a file of this suite
	if err != nil {
		return err
	}
	text := string(raw)
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, redacted)
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil { //nolint:gosec // beneath CF_E2E_ARTIFACTS
		return err
	}
	return os.WriteFile(to, []byte(text), 0o600) //nolint:gosec // beneath CF_E2E_ARTIFACTS
}

// redacted stands for an invite in the artifacts.
const redacted = "<redacted invite>"
