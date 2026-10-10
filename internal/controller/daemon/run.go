package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Options parameterise Start.
type Options struct {
	Paths  config.Paths
	Clock  clock.Clock  // defaults to clock.Real
	Logger *slog.Logger // defaults to slog.Default()
}

// Controller is a running Controller process.
type Controller struct {
	addr net.Addr
	done chan struct{}
	err  error
}

// Addr is the address the API listens on.
func (c *Controller) Addr() net.Addr { return c.addr }

// Wait blocks until the Controller has stopped and releases its resources.
func (c *Controller) Wait() error {
	<-c.done
	return c.err
}

func newAuthority(db *store.DB, ca *pki.CA, clk clock.Clock, host, port string) *pki.Authority {
	return pki.NewAuthority(pki.AuthorityConfig{
		DB: db, CA: ca, Clock: clk, IDs: domain.NewIDGenerator(clk.Now, rand.Reader),
		CertTTL: domain.Defaults().IdentityCertTTL, Host: host, Port: port,
	})
}

// Start opens the database, loads the PKI and serves the API until ctx ends.
func Start(ctx context.Context, o Options) (*Controller, error) {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	p := o.Paths
	cfg, err := config.LoadController(p)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(cfg.Advertise)
	if err != nil {
		return nil, fmt.Errorf("controller.json: advertise: %w", err)
	}
	ca, err := pki.LoadCA(p.PKIDir())
	if err != nil {
		return nil, err
	}
	serverCert, err := ca.LoadServerCert(p.PKIDir(), host, o.Clock.Now())
	if err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, p.DatabaseFile(), store.Options{Clock: o.Clock})
	if err != nil {
		return nil, err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}

	srv := api.New(api.Config{Authority: newAuthority(db, ca, o.Clock, host, port), CA: ca, ServerCert: serverCert, Logger: o.Logger})

	c := &Controller{addr: ln.Addr(), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		c.err = errors.Join(srv.Serve(ctx, ln), db.Close())
	}()
	return c, nil
}
