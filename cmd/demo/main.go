// Command demo is a console for driving the load balancer demo: it brings the
// stack up, then offers the actions one would otherwise type by hand.
//
// It runs on the developer's machine and shells out to docker, so it is a local
// tool and nothing else. It never listens on a socket, and every action it can
// take is fixed in the code.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	composeFile := flag.String("compose", "deploy/docker-compose.yml", "compose file of the demo stack")
	configPath := flag.String("config", "configs/lb.example.yaml", "configuration the balancer reads")
	trafficURL := flag.String("traffic", "http://127.0.0.1:8080/", "address serving proxied traffic")
	statusURL := flag.String("status", "http://127.0.0.1:8081/status", "address serving the status document")
	service := flag.String("service", "loadbalancer", "compose service running the balancer")
	flag.Parse()

	env := &environment{
		composeFile: *composeFile,
		configPath:  *configPath,
		trafficURL:  *trafficURL,
		statusURL:   *statusURL,
		service:     *service,
		client:      &http.Client{Timeout: 10 * time.Second},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, newConsole(), env); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

// run brings the stack up and serves the menu until the operator leaves.
func run(ctx context.Context, con *console, env *environment) error {
	if err := env.preflight(ctx, con); err != nil {
		return err
	}

	current, err := env.bringUp(ctx, con)
	if err != nil {
		return err
	}

	baseline, err := os.ReadFile(env.configPath)
	if err != nil {
		return err
	}

	flow := &traffic{url: env.trafficURL, client: env.client}
	defer flow.halt()

	welcome(con, env, current)

	for ctx.Err() == nil {
		switch choice := con.ask(prompt(ctx, env, flow)); choice {
		case "1":
			toggleTraffic(con, flow)
		case "2":
			measureDistribution(ctx, con, env, flow)
		case "3":
			changeBackend(ctx, con, env, false)
		case "4":
			changeBackend(ctx, con, env, true)
		case "5":
			changeAlgorithm(ctx, con, env)
		case "6":
			demonstrateRateLimit(ctx, con, env, flow)
		case "7":
			showStatus(ctx, con, env)
		case "r":
			reset(ctx, con, env, flow, baseline)
		case "q", "":
			return leave(ctx, con, env, flow, baseline)
		case "?":
			welcome(con, env, mustStatus(ctx, env))
		default:
			con.warn("no action %q — press ? for the list", choice)
		}
	}
	return nil
}

// welcome introduces the console and what it can do.
func welcome(con *console, env *environment, current status) {
	con.heading("Ege-Balancer demo console")
	con.step("traffic      %s", env.trafficURL)
	con.step("status       %s", env.statusURL)
	con.step("grafana      http://localhost:3000/d/ege-balancer")
	con.step("prometheus   http://localhost:9090")
	con.blank()
	con.step("now: %s · %d/%d backends healthy · %d reloads applied",
		con.paint(current.Algorithm, sgrBold), current.Healthy, current.Total, current.Reloads)

	con.heading("Actions")
	con.step("1  start or stop sustained traffic")
	con.step("2  measure the distribution over 30 requests")
	con.step("3  stop a backend            4  start a backend")
	con.step("5  change the algorithm      6  demonstrate the rate limit")
	con.step("7  show the status           r  reset everything")
	con.step("q  quit                      ?  show this again")
	con.note("every action prints the command it runs, so it can be repeated by hand")
}

// prompt renders the menu prompt. It names the algorithm in force, because a
// change made several actions ago is otherwise easy to forget and makes the
// next measurement look wrong.
func prompt(ctx context.Context, env *environment, flow *traffic) string {
	state := mustStatus(ctx, env).Algorithm
	if state == "" {
		state = "unreachable"
	}
	if flow.running() {
		state += " · traffic on"
	}
	return "[" + state + "] action?"
}

// toggleTraffic starts or stops the background stream.
func toggleTraffic(con *console, flow *traffic) {
	if flow.halt() {
		con.ok("sustained traffic stopped")
		return
	}
	flow.start()
	con.ok("sustained traffic started at about %d requests a second", sustainedRate)
	con.note("the dashboard needs a few seconds to catch up: Prometheus scrapes every 5s")
}

// measureDistribution sends a burst and shows which backend served what. The
// background stream is paused first so it cannot skew the count.
func measureDistribution(ctx context.Context, con *console, env *environment, flow *traffic) {
	const requests = 30

	resumed := flow.halt()

	con.echo(fmt.Sprintf("for i in $(seq %d); do curl -s %s; echo; done | sort | uniq -c",
		requests, env.trafficURL))

	bodies, statuses, err := env.measure(ctx, requests)
	if err != nil {
		con.fail("%v", err)
		return
	}
	con.showDistribution(bodies, statuses, requests)

	if resumed {
		flow.start()
		con.note("sustained traffic resumed")
	}
}

// changeBackend stops or starts one of the configured backends.
func changeBackend(ctx context.Context, con *console, env *environment, start bool) {
	current := mustStatus(ctx, env)
	services := env.services(current)
	if len(services) == 0 {
		con.fail("no backends are configured")
		return
	}

	con.blank()
	for i, backend := range current.Backends {
		state := con.paint("healthy", sgrGreen)
		if !backend.Healthy {
			state = con.paint("unhealthy", sgrRed)
		}
		con.step("%2d  %-20s %s", i+1, backend.Addr, state)
	}

	answer := con.ask("which one?")
	index, err := strconv.Atoi(answer)
	if err != nil || index < 1 || index > len(services) {
		con.warn("no backend %q in the list", answer)
		return
	}

	if err := env.setBackendRunning(ctx, con, services[index-1], start); err != nil {
		con.fail("%v", err)
		return
	}
	if start {
		con.ok("%s started — it rejoins the pool after two successful checks", services[index-1])
	} else {
		con.ok("%s stopped — it leaves the pool after three failed checks", services[index-1])
	}
}

// changeAlgorithm switches the balancing algorithm, offering the weighted
// demonstration alongside it.
func changeAlgorithm(ctx context.Context, con *console, env *environment) {
	con.blank()
	con.step("1  round_robin")
	con.step("2  least_connections")
	// Weighted balancing with equal weights behaves exactly like round robin,
	// so choosing it here also gives the first backend a heavier share; there
	// is nothing to see otherwise.
	con.step("3  weighted_round_robin, with the first backend given weight 5")

	switch choice := con.ask("which algorithm?"); choice {
	case "1", "2":
		names := map[string]string{"1": "round_robin", "2": "least_connections"}
		if err := env.setAlgorithm(ctx, con, names[choice]); err != nil {
			con.fail("%v", err)
		}
	case "3":
		current := mustStatus(ctx, env)
		if len(current.Backends) == 0 {
			con.fail("no backends are configured")
			return
		}
		if err := env.setWeightedWithHeavyBackend(ctx, con, current.Backends[0].Addr, 5); err != nil {
			con.fail("%v", err)
			return
		}
		con.note("measure the distribution now: the first backend should take five shares to one")
	default:
		con.warn("no algorithm %q in the list", choice)
	}
}

// demonstrateRateLimit lowers the limit, drives a burst through it, and reports
// what the balancer refused and what the backends were spared.
//
// It restores the limit afterwards, so the demonstration stands on its own and
// leaves nothing behind.
func demonstrateRateLimit(ctx context.Context, con *console, env *environment, flow *traffic) {
	const (
		limit = 5
		burst = 20
	)

	if flow.halt() {
		con.note("sustained traffic stopped: it comes from this machine and would spend the same limit")
	}

	previous, err := currentRateLimit(env.configPath)
	if err != nil {
		con.fail("%v", err)
		return
	}

	if err := env.setRateLimit(ctx, con, limit); err != nil {
		con.fail("%v", err)
		return
	}
	con.ok("limit lowered to %d requests a second", limit)

	served := func() int {
		count, err := env.backendRequests(ctx)
		if err != nil {
			return -1
		}
		return count
	}

	before := served()
	_, statuses, _ := env.measure(ctx, burst)
	reached := served() - before

	con.blank()
	con.step("a burst of %d requests", burst)
	con.ok("%d answered by a backend", statuses[http.StatusOK])
	con.warn("%d refused by the balancer with 429", statuses[http.StatusTooManyRequests])
	if reached >= 0 {
		con.note("the backends served %d of the %d; the rest never reached them", reached, burst)
	}

	// The bucket refills at the configured rate, so a second of quiet buys
	// another burst. This is what separates a token bucket from a plain counter.
	con.blank()
	con.step("after one second of quiet")
	time.Sleep(time.Second)
	_, statuses, _ = env.measure(ctx, limit)
	con.ok("%d answered by a backend", statuses[http.StatusOK])

	if err := env.setRateLimit(ctx, con, previous); err != nil {
		con.fail("%v", err)
		return
	}
	con.ok("limit restored to %d", previous)
}

// currentRateLimit reads the limit the configuration file carries.
func currentRateLimit(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "rate_limit_per_ip:") {
			continue
		}
		value := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		value, _, _ = strings.Cut(value, "#")
		return strconv.Atoi(strings.TrimSpace(value))
	}
	return 0, errors.New("no rate limit in the configuration")
}

// showStatus prints the balancer's own view of the pool.
func showStatus(ctx context.Context, con *console, env *environment) {
	con.echo("curl -s " + env.statusURL)

	current, err := env.status(ctx)
	if err != nil {
		con.fail("%v", err)
		return
	}

	con.blank()
	con.step("%s · %d/%d healthy · %d reloads applied",
		con.paint(current.Algorithm, sgrBold), current.Healthy, current.Total, current.Reloads)
	for _, backend := range current.Backends {
		state := con.paint("healthy", sgrGreen)
		if !backend.Healthy {
			state = con.paint("unhealthy", sgrRed)
		}
		con.step("%-20s weight %d  %-10s %d active", backend.Addr, backend.Weight, state, backend.Active)
	}
}

// reset puts the configuration and the backends back to where they started.
func reset(ctx context.Context, con *console, env *environment, flow *traffic, baseline []byte) {
	flow.halt()
	con.heading("Resetting")

	if err := env.restore(ctx, con, baseline, mustStatus(ctx, env)); err != nil {
		con.fail("%v", err)
		return
	}
	con.ok("configuration restored and every backend started")
}

// leave offers to undo whatever the demo changed before the console exits.
func leave(ctx context.Context, con *console, env *environment, flow *traffic, baseline []byte) error {
	flow.halt()

	// The configuration is restored without asking: leaving the repository with
	// demo settings in it is worse than an extra few seconds on the way out.
	if changed, err := os.ReadFile(env.configPath); err == nil && !bytes.Equal(changed, baseline) {
		con.blank()
		con.step("restoring %s", env.configPath)
		if err := env.restore(ctx, con, baseline, mustStatus(ctx, env)); err != nil {
			con.fail("%v", err)
		}
	}

	if con.confirm("stop the stack?") {
		down := env.compose("down")
		con.echo(down.String())
		return down.run(ctx)
	}
	con.note("the stack is still running; stop it with: docker compose -f %s down", env.composeFile)
	return nil
}

// mustStatus returns the status, or an empty one when the balancer cannot be
// reached. The menu keeps working either way.
func mustStatus(ctx context.Context, env *environment) status {
	current, err := env.status(ctx)
	if err != nil {
		return status{}
	}
	return current
}
