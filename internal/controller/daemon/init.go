package daemon

import (
	"context"
	"crypto/rand"
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
)

// ErrAlreadyInitialized is returned when init finds any trace of a Controller.
var ErrAlreadyInitialized = errors.New("a Controller already exists here")

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
// invitation. It never overwrites: if anything is already there it stops
// before touching it. If it fails midway it removes what it created, so a
// retry starts clean.
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
	if err := refuseExisting(p); err != nil {
		return InitResult{}, err
	}

	for _, dir := range []string{p.ConfigDir, p.DataDir, p.PKIDir()} {
		if err := os.MkdirAll(dir, dirPerms); err != nil {
			return InitResult{}, fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	defer func() {
		if err != nil {
			cleanup(p)
		}
	}()

	db, err := store.Open(ctx, p.DatabaseFile(), store.Options{Clock: o.Clock})
	if err != nil {
		return InitResult{}, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	ca, err := pki.NewCA(o.Clock.Now())
	if err != nil {
		return InitResult{}, err
	}
	if err := ca.Save(p.PKIDir()); err != nil {
		return InitResult{}, err
	}
	if _, err := ca.LoadServerCert(p.PKIDir(), host, o.Clock.Now()); err != nil {
		return InitResult{}, err
	}

	authority := pki.NewAuthority(pki.AuthorityConfig{
		DB: db, CA: ca, Clock: o.Clock, IDs: domain.NewIDGenerator(o.Clock.Now, rand.Reader),
		CertTTL: domain.Defaults().IdentityCertTTL, Host: host, Port: port,
	})
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
	err = client.SaveCredentials(p.CredentialsDir(), client.Credentials{
		Controller: o.Advertise, CertPEM: owner.Certificate, KeyPEM: keyPEM, CAPEM: ca.CertPEM(),
	})
	if err != nil {
		return InitResult{}, err
	}

	ownerPrincipal := domain.Principal{ID: owner.ID, Kind: domain.KindClient}
	invite, until, err := authority.CreateToken(ctx, ownerPrincipal, domain.KindClient, firstInviteTTL)
	if err != nil {
		return InitResult{}, err
	}
	// controller.json is written last: it is what marks the Controller as initialised.
	if err := config.SaveController(p, config.Controller{Listen: o.Listen, Advertise: o.Advertise}); err != nil {
		return InitResult{}, err
	}
	return InitResult{OwnerID: owner.ID, Invite: invite.String(), InviteUntil: until, Paths: p}, nil
}

// refuseExisting fails if init would overwrite anything.
func refuseExisting(p config.Paths) error {
	for _, path := range []string{p.ControllerConfigFile(), p.DatabaseFile(), filepath.Join(p.PKIDir(), "ca.key"), filepath.Join(p.PKIDir(), "ca.pem"), p.CredentialsDir()} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%w: %s exists; nothing was changed", ErrAlreadyInitialized, path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspecting %s: %w", path, err)
		}
	}
	return nil
}

func cleanup(p config.Paths) {
	for _, path := range []string{
		p.PKIDir(), p.CredentialsDir(), p.CredentialsDir() + ".new", p.CredentialsDir() + ".old", p.ControllerConfigFile(),
		p.DatabaseFile(), p.DatabaseFile() + "-wal", p.DatabaseFile() + "-shm",
	} {
		_ = os.RemoveAll(path)
	}
}
