// Package app wires the modules into a running load balancer. Keeping the
// wiring here rather than in main lets the integration tests exercise exactly
// the assembly the binary uses.
package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
	"github.com/berkegemenoguz/ege-balancer/internal/proxy"
	"github.com/berkegemenoguz/ege-balancer/internal/server"
)

// App is an assembled load balancer with its sockets already bound.
type App struct {
	traffic  *server.Server
	metrics  *server.Server
	checker  *health.HTTPChecker
	backends []*balancer.Backend
	strategy balancer.LBStrategy
}

// New assembles the load balancer described by cfg. Both sockets are bound by
// the time it returns, so Addr and MetricsAddr can be read before serving.
func New(cfg *config.Config) (*App, error) {
	strategy, err := balancer.New(cfg.Algorithm)
	if err != nil {
		return nil, err
	}

	backends := balancer.BackendsFromConfig(cfg.Backends)
	checker := health.New(cfg.HealthCheck)

	metrics := observability.NewMetrics()
	pool := observability.NewPool(strategy.Name(), backends, checker)
	metrics.Register(pool)

	traffic, err := server.New(cfg, proxy.New(cfg, strategy, backends, checker, metrics))
	if err != nil {
		return nil, err
	}

	admin, err := server.NewMetrics(cfg, observability.Endpoints(metrics, pool, cfg.EnablePprof))
	if err != nil {
		return nil, err
	}

	return &App{
		traffic:  traffic,
		metrics:  admin,
		checker:  checker,
		backends: backends,
		strategy: strategy,
	}, nil
}

// Addr is the address serving proxied traffic.
func (a *App) Addr() string {
	return a.traffic.Addr()
}

// MetricsAddr is the address serving /metrics and /status.
func (a *App) MetricsAddr() string {
	return a.metrics.Addr()
}

// Run starts health checking and serves both sockets until ctx is cancelled.
// If either server stops on its own, the other is shut down with it rather
// than leaving the process half alive.
func (a *App) Run(ctx context.Context) error {
	ctx, stopAll := context.WithCancel(ctx)
	defer stopAll()

	a.checker.Start(ctx, a.backends)

	slog.Info("load balancer started",
		"listen_addr", a.Addr(), "metrics_addr", a.MetricsAddr(),
		"algorithm", a.strategy.Name(), "backends", len(a.backends))

	servers := []*server.Server{a.traffic, a.metrics}
	failures := make([]error, len(servers))

	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer stopAll()
			failures[i] = srv.Run(ctx)
		}()
	}
	wg.Wait()

	return errors.Join(failures...)
}
