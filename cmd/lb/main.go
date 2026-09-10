// Command lb is the entry point of the load balancer. It reads the
// configuration, assembles the application and serves until it is signalled.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/berkegemenoguz/ege-balancer/internal/app"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// version is stamped in at build time; it is "dev" for a local build.
var version = "dev"

func main() {
	configPath := flag.String("config", "configs/lb.example.yaml", "path to the configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ege-balancer", version)
		return
	}

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
	slog.Info("starting", "version", version)

	balancer, err := app.New(cfg, configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return balancer.Run(ctx, reloadOn(ctx, syscall.SIGHUP))
}

// reloadOn turns the given signals into reload requests for as long as ctx
// lives. SIGHUP is the conventional "re-read your configuration" signal.
func reloadOn(ctx context.Context, signals ...os.Signal) <-chan struct{} {
	received := make(chan os.Signal, 1)
	signal.Notify(received, signals...)

	requests := make(chan struct{}, 1)
	go func() {
		defer signal.Stop(received)
		defer close(requests)

		for {
			select {
			case <-ctx.Done():
				return
			case <-received:
				select {
				case requests <- struct{}{}:
				default:
					// A reload is already queued; one is enough.
				}
			}
		}
	}()
	return requests
}
