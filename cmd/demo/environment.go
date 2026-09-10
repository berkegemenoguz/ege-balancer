package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// status is the part of the balancer's /status document the console uses.
type status struct {
	Algorithm string `json:"algorithm"`
	Reloads   int64  `json:"reloads"`
	Healthy   int    `json:"healthy_backends"`
	Total     int    `json:"total_backends"`
	Backends  []struct {
		Addr    string `json:"addr"`
		Weight  int    `json:"weight"`
		Healthy bool   `json:"healthy"`
		Active  int64  `json:"active_connections"`
	} `json:"backends"`
}

// environment is the demo stack the console drives.
type environment struct {
	composeFile string
	configPath  string
	trafficURL  string
	statusURL   string
	service     string

	client *http.Client
}

// compose builds a docker compose command against the demo stack.
func (e *environment) compose(args ...string) command {
	return command{name: "docker", args: append([]string{"compose", "-f", e.composeFile}, args...)}
}

// preflight checks the one thing the console cannot work around. A missing
// compose or configuration file announces itself clearly enough when compose
// runs, so it is not checked twice here.
func (e *environment) preflight(ctx context.Context, con *console) error {
	con.heading("Checking the environment")

	version := command{name: "docker", args: []string{"version", "--format", "{{.Server.Version}}"}}
	if _, err := version.output(ctx); err != nil {
		con.fail("Docker is not reachable — start Docker Desktop and run this again")
		return err
	}
	con.ok("Docker is running")
	return nil
}

// bringUp starts the stack and waits until the balancer answers.
func (e *environment) bringUp(ctx context.Context, con *console) (status, error) {
	con.heading("Starting the stack")

	up := e.compose("up", "-d", "--build")
	con.echo(up.String())
	if err := up.run(ctx); err != nil {
		return status{}, err
	}

	con.blank()
	con.step("waiting for the balancer to answer on %s", e.statusURL)

	current, err := e.awaitReady(ctx, 90*time.Second)
	if err != nil {
		con.fail("the balancer did not become ready: %v", err)
		return status{}, err
	}

	con.ok("%d of %d backends healthy", current.Healthy, current.Total)
	return current, nil
}

// awaitReady polls the balancer until every backend reports healthy.
func (e *environment) awaitReady(ctx context.Context, timeout time.Duration) (status, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if current, err := e.status(ctx); err == nil && current.Total > 0 &&
			current.Healthy == current.Total {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return status{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return status{}, errors.New("the balancer did not report every backend healthy in time")
}

// status reads the balancer's own view of itself.
func (e *environment) status(ctx context.Context) (status, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.statusURL, nil)
	if err != nil {
		return status{}, err
	}

	response, err := e.client.Do(request)
	if err != nil {
		return status{}, err
	}
	defer func() { _ = response.Body.Close() }()

	var current status
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		return status{}, err
	}
	return current, nil
}

// services returns the compose service name of every configured backend. The
// console only ever acts on a name from this list, so nothing typed at the
// prompt reaches Docker.
func (e *environment) services(current status) []string {
	names := make([]string, 0, len(current.Backends))
	for _, backend := range current.Backends {
		if host, _, found := strings.Cut(backend.Addr, ":"); found {
			names = append(names, host)
		}
	}
	return names
}

// reload asks the balancer to re-read its configuration file and waits until
// the balancer confirms that it applied one.
func (e *environment) reload(ctx context.Context, con *console) error {
	// The count only ever grows, so the reload to wait for is the one that
	// pushes it past what it is now.
	applied := int64(-1)
	if before, err := e.status(ctx); err == nil {
		applied = before.Reloads
	}

	signal := e.compose("kill", "-s", "HUP", e.service)
	con.echo(signal.String())

	if err := signal.run(ctx); err != nil {
		return err
	}
	// "kill" is the name of the compose subcommand for sending a signal; the
	// container keeps running, which is worth saying out loud.
	con.note(`compose reports "Killed" for any signal it sends — the balancer is still running`)

	// The reload happens on the balancer's own goroutine, so wait for it to
	// show up rather than assuming it landed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current, err := e.status(ctx); err == nil && current.Reloads > applied {
			con.ok("configuration reloaded (%s, %d backends, %d reloads applied)",
				current.Algorithm, current.Total, current.Reloads)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	con.warn("the reload was not confirmed within five seconds")
	return nil
}
