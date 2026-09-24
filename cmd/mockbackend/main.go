// Command mockbackend stands in for a real service behind the load balancer in
// the demo environment. One program runs as every backend; each instance is
// given a profile — latency, capacity, answer size, what it remembers about its
// clients, and how it fails — so that the pool behaves like a real one rather
// than answering everything at once.
//
// Every setting is a flag, and every flag can also be set through an
// environment variable named MOCK_ and the flag in capitals, which is what lets
// the compose file share profiles between backends. With -admin, a second port
// changes the backend's faults while it runs and serves its metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// options are the settings that are not part of how the backend behaves.
type options struct {
	seed   uint64
	listen string
	admin  string
}

func main() {
	p, opts, err := parseFlags(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockbackend:", err)
		os.Exit(2)
	}

	if err := run(p, opts); err != nil {
		fmt.Fprintln(os.Stderr, "mockbackend:", err)
		os.Exit(1)
	}
}

// run serves until SIGINT or SIGTERM, then lets the requests in flight finish.
// Handling the signal matters in a container: without it, stopping the backend
// waits out Docker's grace period before the process is killed.
func run(p profile, opts options) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	backend := newServer(p, opts.seed)
	servers := []*http.Server{{
		Addr:              opts.listen,
		Handler:           backend.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}}
	if opts.admin != "" {
		servers = append(servers, &http.Server{
			Addr:              opts.admin,
			Handler:           backend.adminHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	failed := make(chan error, len(servers))
	for _, srv := range servers {
		go func() { failed <- srv.ListenAndServe() }()
	}

	slog.Info("mock backend serving", "name", p.name, "listen", opts.listen, "admin", opts.admin,
		"latency", p.latency, "latency_p99", p.p99, "capacity", p.capacity, "queue", p.queue,
		"body_size", p.bodySize, "error_rate", p.errorRate, "cache_size", p.cacheSize,
		"miss_penalty", p.missPenalty, "hang_rate", p.hangRate, "reset_rate", p.resetRate,
		"drip_rate", p.dripRate, "cold_start", p.coldStart, "cold_factor", p.coldFactor,
		"pause_every", p.pauseEvery, "pause", p.pause, "seed", opts.seed)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	// Hanging and frozen requests have nothing to finish. Letting them go is
	// what allows the requests really being served to drain in time.
	backend.stop()
	backend.clock.thaw()

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	failures := make([]error, 0, len(servers))
	for _, srv := range servers {
		failures = append(failures, srv.Shutdown(shutdown))
	}
	return errors.Join(failures...)
}

// parseFlags reads the profile and the options from args, falling back to
// MOCK_ environment variables looked up through getenv.
func parseFlags(args []string, getenv func(string) string) (profile, options, error) {
	flags := flag.NewFlagSet("mockbackend", flag.ContinueOnError)
	setting := func(name, fallback, usage string) *string {
		if value := getenv("MOCK_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))); value != "" {
			fallback = value
		}
		return flags.String(name, fallback, usage)
	}

	name := setting("name", "", "name the backend answers with (required)")
	listen := setting("listen", ":5678", "address to serve on")
	admin := setting("admin", "", "address of the admin port, which changes faults while the backend runs; empty for none")
	latency := setting("latency", "0s", "median time to serve a request once a worker has it")
	p99 := setting("latency-p99", "0s", "99th percentile of that time; omitted, every request takes latency")
	capacity := setting("capacity", "0", "requests served at once; 0 means no limit")
	queue := setting("queue", "0", "requests that may wait for a worker before the backend answers 503")
	bodySize := setting("body-size", "0", "size of the answer body, such as 4KiB; 0 answers with the name only")
	errorRate := setting("error-rate", "0", "share of requests answered with 500, between 0 and 1")
	cacheSize := setting("cache-size", "0", "client sessions remembered, keyed by the X-Session header; 0 remembers none")
	missPenalty := setting("miss-penalty", "0s", "extra time a request takes when its session is not remembered")
	hangRate := setting("hang-rate", "0", "share of requests held without ever being answered")
	resetRate := setting("reset-rate", "0", "share of requests whose connection is dropped half way through the answer")
	dripRate := setting("drip-rate", "0", "share of requests whose answer is sent in pieces over drip-over")
	dripOver := setting("drip-over", "2s", "how long a dripped answer takes to arrive")
	coldStart := setting("cold-start", "0s", "how long after starting the backend is slower than its profile")
	coldFactor := setting("cold-factor", "1", "how much slower the backend is at the very start of the cold start")
	pauseEvery := setting("pause-every", "0s", "how often the whole process pauses, health check included; 0 never")
	pause := setting("pause", "0s", "how long each pause lasts")
	seed := setting("seed", "0", "seed for latency and failures; 0 picks one at random")

	if err := flags.Parse(args); err != nil {
		return profile{}, options{}, err
	}

	var problems []error
	duration := func(flag, value string) time.Duration {
		d, err := time.ParseDuration(value)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s %q is not a duration", flag, value))
		}
		return d
	}
	count := func(flag, value string) int {
		n, err := strconv.Atoi(value)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s %q is not a whole number", flag, value))
		}
		return n
	}
	number := func(flag, value string) float64 {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s %q is not a number", flag, value))
		}
		return n
	}

	p := profile{
		name:        *name,
		latency:     duration("latency", *latency),
		p99:         duration("latency-p99", *p99),
		capacity:    count("capacity", *capacity),
		queue:       count("queue", *queue),
		errorRate:   number("error-rate", *errorRate),
		cacheSize:   count("cache-size", *cacheSize),
		missPenalty: duration("miss-penalty", *missPenalty),
		hangRate:    number("hang-rate", *hangRate),
		resetRate:   number("reset-rate", *resetRate),
		dripRate:    number("drip-rate", *dripRate),
		dripOver:    duration("drip-over", *dripOver),
		coldStart:   duration("cold-start", *coldStart),
		coldFactor:  number("cold-factor", *coldFactor),
		pauseEvery:  duration("pause-every", *pauseEvery),
		pause:       duration("pause", *pause),
	}

	size, err := parseSize(*bodySize)
	if err != nil {
		problems = append(problems, fmt.Errorf("body-size: %w", err))
	}
	p.bodySize = size

	chosen, err := strconv.ParseUint(*seed, 10, 64)
	if err != nil {
		problems = append(problems, fmt.Errorf("seed %q is not a whole number", *seed))
	}
	if chosen == 0 {
		chosen = rand.Uint64()
	}

	if err := errors.Join(problems...); err != nil {
		return profile{}, options{}, err
	}
	if err := p.validate(); err != nil {
		return profile{}, options{}, err
	}
	return p, options{seed: chosen, listen: *listen, admin: *admin}, nil
}
