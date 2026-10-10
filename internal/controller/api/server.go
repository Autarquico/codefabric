package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/pki"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Config wires a Server.
type Config struct {
	Authority  *pki.Authority
	CA         *pki.CA
	ServerCert tls.Certificate // leaf first, then the CA
	Logger     *slog.Logger    // defaults to slog.Default()
}

// Server is the Controller API over mutual TLS.
type Server struct {
	authority *pki.Authority
	log       *slog.Logger
	tlsConfig *tls.Config
	mux       *http.ServeMux
	routes    map[string]route
}

// New builds the server and its route table.
func New(c Config) *Server {
	log := c.Logger
	if log == nil {
		log = slog.Default()
	}
	roots := x509.NewCertPool()
	roots.AddCert(c.CA.Cert)
	s := &Server{
		authority: c.Authority,
		log:       log,
		tlsConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{c.ServerCert},
			// A certificate is optional at the handshake because /v1/join is
			// anonymous; the route table decides who may call what.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  roots,
		},
		mux:    http.NewServeMux(),
		routes: map[string]route{},
	}
	s.registerRoutes()
	return s
}

// Handler is the authorising handler; tests may serve it with their own TLS.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// TLSConfig is the server-side TLS configuration.
func (s *Server) TLSConfig() *tls.Config { return s.tlsConfig.Clone() }

// Serve accepts TLS connections on ln until ctx ends, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err := srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
