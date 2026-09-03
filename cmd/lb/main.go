// Command lb is the entry point of the load balancer. It wires the internal
// modules together according to the configuration file and runs the server.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

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

	// Until the load balancing engine exists, every request goes to the first
	// configured backend.
	target := cfg.Backends[0]
	srv, err := server.New(cfg, proxy.New(target, cfg.Timeouts))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("listening on %s, forwarding to %s", srv.Addr(), target.Addr)
	return srv.Run(ctx)
}
