// Command lb is the entry point of the load balancer. It reads the
// configuration, assembles the application and serves until it is signalled.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/berkegemenoguz/ege-balancer/internal/app"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
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

	balancer, err := app.New(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return balancer.Run(ctx)
}
