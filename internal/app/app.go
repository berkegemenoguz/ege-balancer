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
	configPath string

	traffic *server.Server
	admin   *server.Server
	checker *health.HTTPChecker
	handler *proxy.Handler
	pool    *observability.Pool
	probes  *observability.Probes
	metrics *observability.Metrics

	// mu guards the state a reload replaces. Requests never take it: they read
	// the handler's own snapshot instead.
	mu       sync.Mutex
	cfg      *config.Config
	backends []*balancer.Backend
}

// New assembles the load balancer described by cfg, which was read from
// configPath. Both sockets are bound by the time it returns, so Addr and
// MetricsAddr can be read before serving.
func New(cfg *config.Config, configPath string) (*App, error) {
	strategy, err := balancer.New(cfg.Algorithm)
	if err != nil {
		return nil, err
	}

	backends := balancer.BackendsFromConfig(cfg.Backends)
	checker := health.New(cfg.HealthCheck)

	metrics := observability.NewMetrics()
	pool := observability.NewPool(strategy.Name(), backends, checker)
	metrics.Register(pool)

	handler := proxy.New(cfg, strategy, backends, checker, metrics)

	traffic, err := server.New(cfg, handler)
	if err != nil {
		return nil, err
	}

	probes := observability.NewProbes(pool)
	admin, err := server.NewMetrics(cfg, observability.Endpoints(metrics, pool, probes, cfg.EnablePprof))
	if err != nil {
		return nil, err
	}

	return &App{
		configPath: configPath,
		traffic:    traffic,
		admin:      admin,
		checker:    checker,
		handler:    handler,
		pool:       pool,
		probes:     probes,
		metrics:    metrics,
		cfg:        cfg,
		backends:   backends,
	}, nil
}

// Reload re-reads the configuration file and applies what can be changed while
// serving. An unreadable or invalid file leaves the balancer running on the
// configuration it already has.
func (a *App) Reload(ctx context.Context) error {
	next, err := config.Load(a.configPath)
	if err != nil {
		slog.Error("reload rejected, keeping the running configuration", "error", err)
		a.metrics.ObserveReload("rejected")
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, warning := range next.Warnings() {
		slog.Warn("configuration", "warning", warning)
	}
	if fixed := a.cfg.RequiresRestart(next); len(fixed) > 0 {
		slog.Warn("some settings need a restart and were not applied", "settings", fixed)
	}

	strategy := a.handler.Strategy()
	if next.Algorithm != a.cfg.Algorithm {
		strategy, err = balancer.New(next.Algorithm)
		if err != nil {
			slog.Error("reload rejected, keeping the running configuration", "error", err)
			a.metrics.ObserveReload("rejected")
			return err
		}
	}

	backends := balancer.MergeBackends(a.backends, next.Backends)

	a.handler.Reload(next, strategy, backends)
	a.checker.Reload(ctx, next.HealthCheck, backends)
	a.pool.Reload(strategy.Name(), backends)

	a.cfg, a.backends = next, backends
	a.metrics.ObserveReload("applied")
	slog.Info("configuration reloaded",
		"algorithm", strategy.Name(), "backends", len(backends),
		"failure_policy", next.FailurePolicy)
	return nil
}

// Addr is the address serving proxied traffic.
func (a *App) Addr() string {
	return a.traffic.Addr()
}

// MetricsAddr is the address serving /metrics, /status, /healthz and /readyz.
func (a *App) MetricsAddr() string {
	return a.admin.Addr()
}

// Run starts health checking and serves both sockets until ctx is cancelled,
// reloading the configuration whenever reload fires. If either server stops on
// its own, the other is shut down with it rather than leaving the process half
// alive.
//
// Once shutdown begins the balancer reports itself not ready, and the metrics
// server stays up until the traffic server has drained, so that whatever routes
// traffic here can see /readyz fail while the requests in flight finish.
func (a *App) Run(ctx context.Context, reload <-chan struct{}) error {
	ctx, stopAll := context.WithCancel(ctx)
	defer stopAll()

	a.checker.Start(ctx, a.backends)
	go a.reloadUntilDone(ctx, reload)
	context.AfterFunc(ctx, a.probes.Drain)

	slog.Info("load balancer started",
		"listen_addr", a.Addr(), "metrics_addr", a.MetricsAddr(),
		"algorithm", a.handler.Strategy().Name(), "backends", len(a.backends))

	adminCtx, stopAdmin := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAdmin()

	var trafficErr, adminErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		defer stopAdmin()
		defer stopAll()
		trafficErr = a.traffic.Run(ctx)
	})
	wg.Go(func() {
		defer stopAll()
		adminErr = a.admin.Run(adminCtx)
	})
	wg.Wait()

	return errors.Join(trafficErr, adminErr)
}

// reloadUntilDone applies a reload each time one is asked for.
func (a *App) reloadUntilDone(ctx context.Context, reload <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-reload:
			if !open {
				return
			}
			// The error is already logged; a bad file must not stop serving.
			_ = a.Reload(ctx)
		}
	}
}
