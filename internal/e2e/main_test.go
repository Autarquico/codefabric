//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// reapTimeout bounds each reaping TestMain does, before and after the tests.
const reapTimeout = time.Minute

// TestMain reaps what earlier invocations that died left, opens the root of
// this one, gets cf, runs the tests and reaps the root again: a test that
// panicked may have run no cleanup.
func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	parent := rootParent()
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
	code := 1
	defer func() { finish(inv, code) }()

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
// CF_E2E_ARTIFACTS says where, and removes it unless CF_E2E_KEEP=1.
func finish(inv *invocation, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	if err := reapRoot(ctx, inv.dir); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: reaping %s: %v\n", inv.dir, err)
	}
	_ = inv.close()
	if dest := os.Getenv(envArtifacts); dest != "" && code != 0 {
		if err := copyLogs(inv.dir, filepath.Join(dest, filepath.Base(inv.dir))); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: copying the logs: %v\n", err)
		}
	}
	if inv.keep {
		fmt.Fprintf(os.Stderr, "e2e: kept %s\n", inv.dir)
		return
	}
	if err := os.RemoveAll(inv.dir); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: removing %s: %v\n", inv.dir, err)
	}
}

// copyLogs copies the manifest, the owner and every logs/ directory of root into dest.
func copyLogs(root, dest string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // what went meanwhile is not copied
		}
		rel, _ := filepath.Rel(root, path)
		wanted := rel == manifestFile || rel == ownerFile || filepath.Base(filepath.Dir(path)) == "logs"
		if d.IsDir() || !d.Type().IsRegular() || !wanted {
			return nil
		}
		return copyFile(path, filepath.Join(dest, rel))
	})
}

func copyFile(from, to string) (err error) {
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil { //nolint:gosec // beneath CF_E2E_ARTIFACTS
		return err
	}
	in, err := os.Open(from) //nolint:gosec // a file of this suite
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to) //nolint:gosec // beneath CF_E2E_ARTIFACTS
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}
