// Package server accepts incoming TCP/HTTP connections, manages the connection
// lifecycle and performs graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	http     *http.Server
	listener net.Listener
}

// New binds the address from the configuration and prepares an HTTP server that
// serves handler under the configured timeouts. The socket is open once New
// returns, so a caller can read Addr before serving starts.
func New(cfg *config.Config, handler http.Handler) (*Server, error) {
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	return &Server{
		listener: listener,
		http: &http.Server{
			Handler:      handler,
			ReadTimeout:  time.Duration(cfg.Timeouts.ReadTimeout),
			WriteTimeout: time.Duration(cfg.Timeouts.WriteTimeout),
			IdleTimeout:  time.Duration(cfg.Timeouts.IdleTimeout),
		},
	}, nil
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
	log.Printf("shutting down, waiting up to %s for in-flight requests", shutdownGrace)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
