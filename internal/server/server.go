// Package server accepts incoming TCP/HTTP connections, manages the connection
// lifecycle and performs graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// shutdownGrace is how long in-flight requests may take to finish once a
// shutdown has been requested.
const shutdownGrace = 30 * time.Second

// Server owns the listening socket and the HTTP server on top of it.
type Server struct {
	name     string
	http     *http.Server
	listener net.Listener
}

// New prepares the server that carries proxied traffic, under the configured
// timeouts and connection limit. The socket is open once New returns, so a
// caller can read Addr before serving starts.
func New(cfg *config.Config, handler http.Handler) (*Server, error) {
	listener, err := listen(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}

	return &Server{
		name:     "proxy",
		listener: newLimitListener(listener, cfg.Limits.MaxConnections),
		http: &http.Server{
			Handler:      handler,
			ReadTimeout:  time.Duration(cfg.Timeouts.ReadTimeout),
			WriteTimeout: time.Duration(cfg.Timeouts.WriteTimeout),
			IdleTimeout:  time.Duration(cfg.Timeouts.IdleTimeout),
		},
	}, nil
}

// NewMetrics prepares the server that exposes /metrics and /status. It carries
// no connection limit: observability must keep working precisely when the
// traffic port is saturated.
func NewMetrics(cfg *config.Config, handler http.Handler) (*Server, error) {
	listener, err := listen(cfg.MetricsAddr)
	if err != nil {
		return nil, err
	}

	return &Server{
		name:     "metrics",
		listener: listener,
		http: &http.Server{
			Handler:      handler,
			ReadTimeout:  time.Duration(cfg.Timeouts.ReadTimeout),
			WriteTimeout: time.Duration(cfg.Timeouts.WriteTimeout),
			IdleTimeout:  time.Duration(cfg.Timeouts.IdleTimeout),
		},
	}, nil
}

// listen binds addr for serving.
func listen(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return listener, nil
}

// Addr is the address the server actually listens on, which differs from the
// configured one when the port was left to the operating system.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Run serves requests until ctx is cancelled, then stops accepting new
// connections and waits for the in-flight ones to finish.
func (s *Server) Run(ctx context.Context) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.http.Serve(s.listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		return s.shutdown()
	}
}

// shutdown drains the server, forcing the remaining connections closed if they
// outlast the grace period.
func (s *Server) shutdown() error {
	slog.Info("shutting down", "server", s.name, "grace", shutdownGrace)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
