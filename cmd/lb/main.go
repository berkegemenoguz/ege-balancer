// Command lb is the entry point of the load balancer. It wires the internal
// modules together according to the configuration file and runs the server.
package main

import (
	"flag"
	"log"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func main() {
	configPath := flag.String("config", "configs/lb.example.yaml", "path to the configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("loaded %s: %s algorithm, %d backends, listening on %s",
		*configPath, cfg.Algorithm, len(cfg.Backends), cfg.ListenAddr)
}
