// Package config resolves where Code Flock keeps its files and reads the
// Controller's own settings.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/Autarquico/codeflock/internal/domain"
)

const (
	appDir       = "codeflock"
	legacyAppDir = "codefabric"
)

// Paths are the effective locations of Code Flock's files (§35).
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

// WorkerSecretsFile is the catalog of the secrets this machine holds as a
// Worker (§16.7). It lives with the Worker's credentials, in its private
// configuration, not with the data a backup or a cleanup of runs touches.
func (p Paths) WorkerSecretsFile() string {
	return filepath.Join(p.ConfigDir, "worker", "secrets.json")
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

// ClientConfigFile is the client's own settings file (what it trusts, not what
// it is: the credentials are apart).
func (p Paths) ClientConfigFile() string { return filepath.Join(p.ConfigDir, "client.json") }

// ControllerConfigFile is the Controller's settings file.
func (p Paths) ControllerConfigFile() string { return filepath.Join(p.ConfigDir, "controller.json") }

// DatabaseFile is the Controller's SQLite database.
func (p Paths) DatabaseFile() string { return filepath.Join(p.DataDir, "controller.db") }

// GCLockFile is the lock that the garbage collector holds while it deletes blobs
// and that a backup holds while it copies them.
func (p Paths) GCLockFile() string { return filepath.Join(p.DataDir, "gc.lock") }

// PKIDir holds the CA and the server certificate.
func (p Paths) PKIDir() string { return filepath.Join(p.DataDir, "pki") }

// Located is Paths with whether each directory is the default one, that is,
// not given by a variable of the user: only a default directory is migrated
// from its pre-rename name; one given by hand is respected as it is.
type Located struct {
	Paths
	ConfigIsDefault bool
	DataIsDefault   bool
}

// LegacyOf is the directory a default directory had before the rename of
// CodeFabric to Code Flock: the same place under the old application name.
func LegacyOf(dir string) string { return filepath.Join(filepath.Dir(dir), legacyAppDir) }

// legacyVarWarning is where the use of a CF_* variable is reported, and
// warnedVars what has been reported already: once per process and variable.
var (
	legacyVarWarning io.Writer = os.Stderr
	warnedVars       sync.Map
)

func warnLegacyVar(old, current string) {
	if _, seen := warnedVars.LoadOrStore(old, true); !seen {
		_, _ = fmt.Fprintf(legacyVarWarning, "cflk: %s is deprecated and will stop being read in 0.5; use %s\n", old, current)
	}
}

// Resolve is Locate, keeping the old directories while they are in use: a
// default directory that does not exist, next to the one it had before the
// rename that does, is the old one. Nothing moves them while a service of the
// old name is installed (its install-service does), and until then that
// service, and the client with it, go on with what they have. It says so once
// per process, naming the service.
func Resolve(getenv func(string) string, home func() (string, error)) (Paths, error) {
	l, err := Locate(getenv, home)
	if err != nil {
		return Paths{}, err
	}
	var kept bool
	keep := func(dir *string, isDefault bool) {
		if !isDefault {
			return
		}
		if _, err := os.Lstat(*dir); err == nil {
			return
		}
		if old := LegacyOf(*dir); isDir(old) {
			*dir, kept = old, true
		}
	}
	keep(&l.ConfigDir, l.ConfigIsDefault)
	keep(&l.DataDir, l.DataIsDefault)
	if kept {
		warnOldUnits(home)
	}
	return l.Paths, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func warnOldUnits(home func() (string, error)) {
	h, err := home()
	if err != nil {
		return
	}
	for _, u := range OldUnits(h, runtime.GOOS) {
		if _, seen := warnedVars.LoadOrStore("unit "+u.Name, true); !seen {
			_, _ = fmt.Fprintf(legacyVarWarning, "cflk: the service %s still uses the old codefabric directories, so they were not moved; "+
				"run `cflk %s install-service` to move them with the service\n", u.Name, u.Kind)
		}
	}
}

// Override is the variable that gives a directory and its value: the CFLK_*
// one if it is set, else the CF_* one it had before the rename, else the first
// with an empty value. It is the one rule of precedence, for the environment of
// the process (Locate) and for that of a service unit alike.
func Override(getenv func(string) string, override, legacyOverride string) (name, value string) {
	if v := getenv(override); v != "" {
		return override, v
	}
	if v := getenv(legacyOverride); v != "" {
		return legacyOverride, v
	}
	return override, ""
}

// Locate applies, per directory: the CFLK_* override, then the CF_* one (read
// with a warning until 0.5), then the XDG variable, then the XDG default under
// the home directory. It uses the XDG layout on every OS so a CLI user finds
// the same paths everywhere; see ADR 0002 D8. Relative values are ignored, as
// the XDG specification requires.
func Locate(getenv func(string) string, home func() (string, error)) (Located, error) {
	dir := func(override, legacyOverride, xdgVar, homeRel string) (path string, isDefault bool, err error) {
		name, v := Override(getenv, override, legacyOverride)
		if name == legacyOverride {
			warnLegacyVar(legacyOverride, override)
		}
		if v != "" {
			if !filepath.IsAbs(v) {
				return "", false, fmt.Errorf("config: %s must be an absolute path, got %q", name, v)
			}
			return filepath.Clean(v), false, nil
		}
		if v := getenv(xdgVar); filepath.IsAbs(v) {
			return filepath.Join(v, appDir), true, nil
		}
		h, err := home()
		if err != nil {
			return "", false, fmt.Errorf("config: locating the home directory: %w", err)
		}
		return filepath.Join(h, homeRel, appDir), true, nil
	}
	cfg, cfgDefault, err := dir("CFLK_CONFIG_DIR", "CF_CONFIG_DIR", "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Located{}, err
	}
	data, dataDefault, err := dir("CFLK_DATA_DIR", "CF_DATA_DIR", "XDG_DATA_HOME", filepath.Join(".local", "share"))
	if err != nil {
		return Located{}, err
	}
	return Located{Paths: Paths{ConfigDir: cfg, DataDir: data}, ConfigIsDefault: cfgDefault, DataIsDefault: dataDefault}, nil
}

// ResolveFromEnv is Resolve over the real environment.
func ResolveFromEnv() (Paths, error) {
	return Resolve(os.Getenv, os.UserHomeDir)
}

// ErrNotInitialized is returned when a file that init creates does not exist.
var ErrNotInitialized = errors.New("config: not initialized")
