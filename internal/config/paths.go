// Package config resolves where CodeFabric keeps its files and reads the
// Controller's own settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/domain"
)

const appDir = "codefabric"

// Paths are the effective locations of CodeFabric's files (§35).
type Paths struct {
	ConfigDir string // client credentials and controller.json
	DataDir   string // controller.db and the PKI
}

// CredentialsDir holds the client's certificate, key and pinned CA.
func (p Paths) CredentialsDir() string { return filepath.Join(p.ConfigDir, "credentials") }

// WorkerCredentialsDir holds the credentials of the Worker daemon of this
// machine, apart from the client's: one machine can be both.
func (p Paths) WorkerCredentialsDir() string {
	return filepath.Join(p.ConfigDir, "worker", "credentials")
}

// WorkerDataDir is where the Worker daemon keeps its own files. It is a
// directory of its own under DataDir, not DataDir itself (§22.5): one machine
// can be the Controller and a Worker with the same data directory.
func (p Paths) WorkerDataDir() string { return filepath.Join(p.DataDir, "worker") }

// WorkerStateFile is the Worker's durable state (§6.4).
func (p Paths) WorkerStateFile() string { return filepath.Join(p.WorkerDataDir(), "state.db") }

// WorkerRunDir holds what the Worker keeps of one Run under one lease epoch: the
// launch file, the markers of its wrapper and its output (§22.5). It is named
// like the session of the Runtime, by Run and epoch, so that two epochs of a Run
// never share markers.
func (p Paths) WorkerRunDir(run string, leaseEpoch uint64) string {
	return filepath.Join(p.WorkerDataDir(), "runs", domain.RunEpoch(run, leaseEpoch))
}

// WorkerWorktreeDir is the isolated worktree of one Run under one lease epoch (§22.5).
func (p Paths) WorkerWorktreeDir(run string, leaseEpoch uint64) string {
	return filepath.Join(p.WorkerDataDir(), "worktrees", domain.RunEpoch(run, leaseEpoch))
}

// WorkerRepoDir is the bare repository of a Project on this Worker, shared by
// the worktrees of its Runs (§22.5).
func (p Paths) WorkerRepoDir(project string) string {
	return filepath.Join(p.WorkerDataDir(), "repos", project+".git")
}

// RelayDir holds the git relay of the Controller (§22.1): the bare repositories
// of the Projects, the hooks they share and the scratch space of the transport.
func (p Paths) RelayDir() string { return filepath.Join(p.DataDir, "relay") }

// ControllerConfigFile is the Controller's settings file.
func (p Paths) ControllerConfigFile() string { return filepath.Join(p.ConfigDir, "controller.json") }

// DatabaseFile is the Controller's SQLite database.
func (p Paths) DatabaseFile() string { return filepath.Join(p.DataDir, "controller.db") }

// PKIDir holds the CA and the server certificate.
func (p Paths) PKIDir() string { return filepath.Join(p.DataDir, "pki") }

// Resolve applies, per directory: the CF_* override, then the XDG variable,
// then the XDG default under the home directory. It uses the XDG layout on
// every OS so a CLI user finds the same paths everywhere; see ADR 0002 D8.
// Relative values are ignored, as the XDG specification requires.
func Resolve(getenv func(string) string, home func() (string, error)) (Paths, error) {
	dir := func(override, xdgVar, homeRel string) (string, error) {
		if v := getenv(override); v != "" {
			if !filepath.IsAbs(v) {
				return "", fmt.Errorf("config: %s must be an absolute path, got %q", override, v)
			}
			return filepath.Clean(v), nil
		}
		if v := getenv(xdgVar); filepath.IsAbs(v) {
			return filepath.Join(v, appDir), nil
		}
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("config: locating the home directory: %w", err)
		}
		return filepath.Join(h, homeRel, appDir), nil
	}
	cfg, err := dir("CF_CONFIG_DIR", "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Paths{}, err
	}
	data, err := dir("CF_DATA_DIR", "XDG_DATA_HOME", filepath.Join(".local", "share"))
	if err != nil {
		return Paths{}, err
	}
	return Paths{ConfigDir: cfg, DataDir: data}, nil
}

// ResolveFromEnv is Resolve over the real environment.
func ResolveFromEnv() (Paths, error) {
	return Resolve(os.Getenv, os.UserHomeDir)
}

// ErrNotInitialized is returned when a file that init creates does not exist.
var ErrNotInitialized = errors.New("config: not initialized")
