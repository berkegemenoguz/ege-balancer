// Command mockbackend stands in for a real service behind the load balancer in
// the demo environment. One program runs as every backend; each instance is
// given a profile — latency, capacity, answer size, error rate — so that the
// pool behaves like a real one rather than answering everything at once.
//
// Every setting is a flag, and every flag can also be set through an
// environment variable named MOCK_ and the flag in capitals, which is what lets
// the compose file share profiles between backends.
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

func main() {
	p, seed, listen, err := parseFlags(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockbackend:", err)
		os.Exit(2)
	}

	if err := run(p, seed, listen); err != nil {
		fmt.Fprintln(os.Stderr, "mockbackend:", err)
		os.Exit(1)
	}
}

// run serves until SIGINT or SIGTERM, then lets the requests in flight finish.
// Handling the signal matters in a container: without it, stopping the backend
// waits out Docker's grace period before the process is killed.
func run(p profile, seed uint64, listen string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              listen,
		Handler:           newServer(p, seed).handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()

	slog.Info("mock backend serving", "name", p.name, "listen", listen,
		"latency", p.latency, "latency_p99", p.p99, "capacity", p.capacity, "queue", p.queue,
		"body_size", p.bodySize, "error_rate", p.errorRate, "seed", seed)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// parseFlags reads the profile, the seed and the listen address from args,
// falling back to MOCK_ environment variables looked up through getenv.
func parseFlags(args []string, getenv func(string) string) (profile, uint64, string, error) {
	flags := flag.NewFlagSet("mockbackend", flag.ContinueOnError)
	setting := func(name, fallback, usage string) *string {
		if value := getenv("MOCK_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))); value != "" {
			fallback = value
		}
		return flags.String(name, fallback, usage)
	}

	name := setting("name", "", "name the backend answers with (required)")
	listen := setting("listen", ":5678", "address to serve on")
	latency := setting("latency", "0s", "median time to serve a request once a worker has it")
	p99 := setting("latency-p99", "0s", "99th percentile of that time; omitted, every request takes latency")
	capacity := setting("capacity", "0", "requests served at once; 0 means no limit")
	queue := setting("queue", "0", "requests that may wait for a worker before the backend answers 503")
	bodySize := setting("body-size", "0", "size of the answer body, such as 4KiB; 0 answers with the name only")
	errorRate := setting("error-rate", "0", "share of requests answered with 500, between 0 and 1")
	seed := setting("seed", "0", "seed for latency and failures; 0 picks one at random")

	if err := flags.Parse(args); err != nil {
		return profile{}, 0, "", err
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

	p := profile{
		name:     *name,
		latency:  duration("latency", *latency),
		p99:      duration("latency-p99", *p99),
		capacity: count("capacity", *capacity),
		queue:    count("queue", *queue),
	}

	size, err := parseSize(*bodySize)
	if err != nil {
		problems = append(problems, fmt.Errorf("body-size: %w", err))
	}
	p.bodySize = size

	if p.errorRate, err = strconv.ParseFloat(*errorRate, 64); err != nil {
		problems = append(problems, fmt.Errorf("error-rate %q is not a number", *errorRate))
	}

	chosen, err := strconv.ParseUint(*seed, 10, 64)
	if err != nil {
		problems = append(problems, fmt.Errorf("seed %q is not a whole number", *seed))
	}
	if chosen == 0 {
		chosen = rand.Uint64()
	}

	if err := errors.Join(problems...); err != nil {
		return profile{}, 0, "", err
	}
	if err := p.validate(); err != nil {
		return profile{}, 0, "", err
	}
	return p, chosen, *listen, nil
}
