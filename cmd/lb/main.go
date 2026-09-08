// Command lb is the entry point of the load balancer. It wires the internal
// modules together according to the configuration file and runs the server.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
	"github.com/berkegemenoguz/ege-balancer/internal/proxy"
	"github.com/berkegemenoguz/ege-balancer/internal/server"
)

func main() {
	configPath := flag.String("config", "configs/lb.example.yaml", "path to the configuration file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		// The structured logger may not exist yet when configuration fails.
		log.Fatal(err)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	observability.NewLogger(cfg.Logging)

	strategy, err := balancer.New(cfg.Algorithm)
	if err != nil {
		return err
	}
	backends := balancer.BackendsFromConfig(cfg.Backends)
	checker := health.New(cfg.HealthCheck)

	metrics := observability.NewMetrics()
	pool := observability.NewPool(strategy.Name(), backends, checker)
	metrics.Register(pool)

	traffic, err := server.New(cfg, proxy.New(cfg, strategy, backends, checker, metrics))
	if err != nil {
		return err
	}
	admin, err := server.NewMetrics(cfg, observability.Endpoints(metrics, pool))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	checker.Start(ctx, backends)

	slog.Info("load balancer started",
		"listen_addr", traffic.Addr(), "metrics_addr", admin.Addr(),
		"algorithm", strategy.Name(), "backends", len(backends))

	return runAll(ctx, traffic, admin)
}

// runAll serves every server until the context is cancelled, and reports the
// problems any of them ran into. If one server stops on its own, the others are
// shut down with it rather than leaving the process half alive.
func runAll(ctx context.Context, servers ...*server.Server) error {
	ctx, stopAll := context.WithCancel(ctx)
	defer stopAll()

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
