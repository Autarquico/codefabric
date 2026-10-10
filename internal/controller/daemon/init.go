package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	firstInviteTTL = time.Hour
	dirPerms       = 0o700
	filePerms      = 0o600
	initLockName   = ".init.lock"
)

var (
	// ErrAlreadyInitialized is returned when init finds any trace of a Controller.
	ErrAlreadyInitialized = errors.New("a Controller already exists here")
	// ErrInitInProgress is returned when another init holds the lock, or died holding it.
	ErrInitInProgress = errors.New("another `cf controller init` is running here")
	// ErrUnsafeDirectory is returned when the data or config directory is open to others.
	ErrUnsafeDirectory = errors.New("unsafe directory")
)

// InitOptions parameterise Init.
type InitOptions struct {
	Paths     config.Paths
	Clock     clock.Clock // defaults to clock.Real
	Advertise string      // host:port written into invitations and the server certificate
	Listen    string      // address `cf controller run` binds
	OwnerName string      // name recorded for the owner's machine
}

// InitResult is what init tells the operator.
type InitResult struct {
	OwnerID     domain.ID
	Invite      string // first invitation, for the next machine
	InviteUntil time.Time
	Paths       config.Paths
}

// Init creates the Controller's state: data and config directories, database,
// CA, server certificate, the owner's credential bundle and a first client
// invitation. It never overwrites: it holds an exclusive lock while it checks
// and writes, and stops before touching anything if a trace of a Controller is
// already there. If it fails midway it removes exactly what this run created,
// so a retry starts clean and a concurrent winner is never harmed.
func Init(ctx context.Context, o InitOptions) (_ InitResult, err error) {
	host, port, err := net.SplitHostPort(o.Advertise)
	if n, convErr := strconv.Atoi(port); err != nil || host == "" || convErr != nil || n < 1 || n > 65535 {
		return InitResult{}, fmt.Errorf("--advertise must be host:port, got %q", o.Advertise)
	}
	if o.Listen == "" {
		o.Listen = net.JoinHostPort("0.0.0.0", port)
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	p := o.Paths
	if err := requirePrivate(p.DataDir, p.ConfigDir); err != nil {
		return InitResult{}, err
	}

	var made created
	defer func() {
		if err != nil {
			made.rollBack()
		}
	}()

	// The data directory is created first because the lock lives in it.
	if err := made.mkdir(p.DataDir); err != nil {
		return InitResult{}, err
	}
	lock := filepath.Join(p.DataDir, initLockName)
	if err := made.file(lock); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return InitResult{}, fmt.Errorf("%w: %s exists; if no init is running, remove it", ErrInitInProgress, lock)
		}
		return InitResult{}, err
	}
	if err := refuseExisting(p); err != nil {
		return InitResult{}, err
	}
	if err := made.mkdir(p.ConfigDir); err != nil {
		return InitResult{}, err
	}
	if err := made.mkdir(p.PKIDir()); err != nil {
		return InitResult{}, err
	}

	// SQLite gives -wal and -shm the mode of the database file.
	made.track(p.DatabaseFile() + "-shm")
	made.track(p.DatabaseFile() + "-wal")
	if err := made.file(p.DatabaseFile()); err != nil {
		return InitResult{}, err
	}
	db, err := store.Open(ctx, p.DatabaseFile(), store.Options{Clock: o.Clock})
	if err != nil {
		return InitResult{}, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	pkiDir := p.PKIDir()
	for _, name := range []string{"ca.key", "ca.pem", "server.key", "server.pem"} {
		made.track(filepath.Join(pkiDir, name))
	}
	ca, err := pki.NewCA(o.Clock.Now())
	if err != nil {
		return InitResult{}, err
	}
	if err := ca.Save(pkiDir); err != nil {
		return InitResult{}, err
	}
	if _, err := ca.LoadServerCert(pkiDir, host, o.Clock.Now()); err != nil {
		return InitResult{}, err
	}

	authority := newAuthority(db, ca, o.Clock, host, port)
	key, csr, err := client.GenerateKeyAndCSR()
	if err != nil {
		return InitResult{}, err
	}
	keyPEM, err := client.EncodeKey(key)
	if err != nil {
		return InitResult{}, err
	}
	owner, err := authority.BootstrapOwner(ctx, csr, o.OwnerName)
	if err != nil {
		return InitResult{}, err
	}
	for _, suffix := range []string{"", ".new", ".old"} {
		made.track(p.CredentialsDir() + suffix)
	}
	err = client.SaveCredentials(p.CredentialsDir(), client.Credentials{
		Controller: o.Advertise, CertPEM: owner.Certificate, KeyPEM: keyPEM, CAPEM: ca.CertPEM(),
	})
	if err != nil {
		return InitResult{}, err
	}

	ownerSession, err := owner.Session()
	if err != nil {
		return InitResult{}, err
	}
	invite, until, err := authority.CreateToken(ctx, ownerSession, domain.KindClient, firstInviteTTL)
	if err != nil {
		return InitResult{}, err
	}
	// controller.json is written last: it is what marks the Controller as initialised.
	made.track(p.ControllerConfigFile())
	if err := config.SaveController(p, config.Controller{Listen: o.Listen, Advertise: o.Advertise}); err != nil {
		return InitResult{}, err
	}
	_ = os.Remove(lock) // best effort: a Controller that exists makes any later init refuse anyway
	return InitResult{OwnerID: owner.ID, Invite: invite.String(), InviteUntil: until, Paths: p}, nil
}

// requirePrivate refuses existing directories that others can read or enter:
// the data directory holds the CA key and the config directory the credentials.
func requirePrivate(dirs ...string) error {
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspecting %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrUnsafeDirectory, dir)
		}
		if info.Mode().Perm()&^dirPerms != 0 {
			return fmt.Errorf("%w: %s has mode %04o and would hold secrets; run `chmod 700 %s` and retry",
				ErrUnsafeDirectory, dir, info.Mode().Perm(), dir)
		}
	}
	return nil
}

// refuseExisting fails if init would overwrite anything.
func refuseExisting(p config.Paths) error {
	if pki.Exists(p.PKIDir()) {
		return fmt.Errorf("%w: %s holds a CA or server certificate; nothing was changed", ErrAlreadyInitialized, p.PKIDir())
	}
	for _, path := range []string{
		p.ControllerConfigFile(), p.DatabaseFile(),
		p.CredentialsDir(), p.CredentialsDir() + ".new", p.CredentialsDir() + ".old",
	} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%w: %s exists; nothing was changed", ErrAlreadyInitialized, path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspecting %s: %w", path, err)
		}
	}
	return nil
}

// created remembers what this run brought into existence, in order, so a
// failure removes that and nothing else.
type created struct {
	dirs  []string // removed only if empty: a concurrent run may have put its own files inside
	paths []string // removed with everything beneath them
}

// mkdir creates dir (and any missing parents, which are left in place on
// rollback). A directory that already exists is not ours and is not recorded.
func (c *created) mkdir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), dirPerms); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dir), err)
	}
	err := os.Mkdir(dir, dirPerms)
	switch {
	case err == nil:
		c.dirs = append(c.dirs, dir)
	case errors.Is(err, fs.ErrExist):
		return requirePrivate(dir)
	default:
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}

// file creates an empty private file that must not exist yet (O_EXCL) and records it.
func (c *created) file(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerms) //nolint:gosec // path is built from the resolved Paths
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	c.paths = append(c.paths, path)
	return f.Close()
}

// track records a path this run is about to create, unless something is
// already there: that would not be ours to remove.
func (c *created) track(path string) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		c.paths = append(c.paths, path)
	}
}

func (c *created) rollBack() {
	for i := len(c.paths) - 1; i >= 0; i-- {
		_ = os.RemoveAll(c.paths[i])
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		_ = os.Remove(c.dirs[i])
	}
}
