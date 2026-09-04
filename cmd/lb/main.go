// Command lb is the entry point of the load balancer. It wires the internal
// modules together according to the configuration file and runs the server.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/proxy"
	"github.com/berkegemenoguz/ege-balancer/internal/server"
)

func main() {
	configPath := flag.String("config", "configs/lb.example.yaml", "path to the configuration file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		log.Fatal(err)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	strategy, err := balancer.New(cfg.Algorithm)
	if err != nil {
		return err
	}
	backends := balancer.BackendsFromConfig(cfg.Backends)

	srv, err := server.New(cfg, proxy.New(strategy, backends, cfg.Timeouts))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("listening on %s, balancing %d backends with %s",
		srv.Addr(), len(backends), strategy.Name())
	return srv.Run(ctx)
}
