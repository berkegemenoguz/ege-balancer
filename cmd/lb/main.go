// Command lb is the entry point of the load balancer. It wires the internal
// modules together according to the configuration file and runs the server.
package main

import (
	"flag"
	"log"
)

func main() {
	configPath := flag.String("config", "configs/lb.example.yaml", "path to the configuration file")
	flag.Parse()

	log.Printf("ege-balancer starting with config %q", *configPath)
}
