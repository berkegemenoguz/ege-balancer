package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sustainedRate is the request rate of the background traffic. It is enough to
// give the dashboard a readable plateau without saturating anything.
const sustainedRate = 20

// traffic keeps a steady stream of requests running until it is stopped, so
// that the dashboard has something to show while someone is talking over it.
type traffic struct {
	url    string
	client *http.Client

	mu     sync.Mutex
	stop   chan struct{}
	closed chan struct{}
}

// running reports whether traffic is flowing.
func (t *traffic) running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stop != nil
}

// start begins the stream. Starting an already running stream does nothing.
func (t *traffic) start() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stop != nil {
		return
	}
	stop, closed := make(chan struct{}), make(chan struct{})
	t.stop, t.closed = stop, closed

	go func() {
		defer close(closed)
		ticker := time.NewTicker(time.Second / sustainedRate)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				go t.once()
			}
		}
	}()
}

// halt stops the stream and waits for it to finish. It is safe to call when
// nothing is running, which is what lets other actions clear the way for
// themselves without checking first.
func (t *traffic) halt() bool {
	t.mu.Lock()
	stop, closed := t.stop, t.closed
	t.stop, t.closed = nil, nil
	t.mu.Unlock()

	if stop == nil {
		return false
	}
	close(stop)
	<-closed
	return true
}

// once sends a single request and discards the answer.
func (t *traffic) once() {
	response, err := t.client.Get(t.url)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

// measure sends count requests and reports which backend answered each one.
// The distribution comes from the responses themselves, so it is what the
// client saw rather than what a counter says.
func (e *environment) measure(ctx context.Context, count int) (map[string]int, map[int]int, error) {
	const workers = 8

	var (
		mu       sync.Mutex
		bodies   = make(map[string]int)
		statuses = make(map[int]int)
		wg       sync.WaitGroup
	)

	requests := make(chan struct{}, count)
	for range count {
		requests <- struct{}{}
	}
	close(requests)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requests {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.trafficURL, nil)
				if err != nil {
					return
				}
				response, err := e.client.Do(request)
				if err != nil {
					mu.Lock()
					statuses[0]++
					mu.Unlock()
					continue
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()

				mu.Lock()
				statuses[response.StatusCode]++
				bodies[strings.TrimSpace(string(body))]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return bodies, statuses, nil
}

// showDistribution prints the measured distribution as a bar per backend.
func (c *console) showDistribution(bodies map[string]int, statuses map[int]int, total int) {
	names := make([]string, 0, len(bodies))
	highest := 0
	for name, count := range bodies {
		names = append(names, name)
		if count > highest {
			highest = count
		}
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })

	c.blank()
	for _, name := range names {
		count := bodies[name]
		width := 0
		if highest > 0 {
			width = count * 32 / highest
		}
		label := name
		if label == "" {
			label = "(no body)"
		}
		c.printf("  %-14s %s %d\n", label, c.paint(strings.Repeat("█", width), sgrGreen), count)
	}

	codes := make([]int, 0, len(statuses))
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	seen := make([]string, 0, len(codes))
	for _, code := range codes {
		seen = append(seen, fmt.Sprintf("%d × %d", statuses[code], code))
	}
	c.blank()
	c.step("%d requests: %s", total, strings.Join(seen, ", "))
}

// naturalLess orders names by their trailing number, so that backend-10 comes
// after backend-9 rather than after backend-1.
func naturalLess(a, b string) bool {
	prefixA, numberA := splitTrailingNumber(a)
	prefixB, numberB := splitTrailingNumber(b)
	if prefixA != prefixB {
		return prefixA < prefixB
	}
	return numberA < numberB
}

// splitTrailingNumber separates a name from the number it ends with.
func splitTrailingNumber(name string) (string, int) {
	end := len(name)
	for end > 0 && name[end-1] >= '0' && name[end-1] <= '9' {
		end--
	}
	number, err := strconv.Atoi(name[end:])
	if err != nil {
		return name, -1
	}
	return name[:end], number
}

// setBackendRunning stops or starts one backend service.
func (e *environment) setBackendRunning(ctx context.Context, con *console, service string, running bool) error {
	action := "stop"
	if running {
		action = "start"
	}

	change := e.compose(action, service)
	con.echo(change.String())
	return change.run(ctx)
}

// applyConfig rewrites the configuration file and asks the balancer to reload
// it, which is exactly what an operator would do by hand.
func (e *environment) applyConfig(ctx context.Context, con *console, edits ...func([]byte) ([]byte, error)) error {
	content, err := os.ReadFile(e.configPath)
	if err != nil {
		return err
	}

	// Every edit is applied before the single reload, so a change that needs
	// two of them does not make the balancer reload twice.
	for _, edit := range edits {
		if content, err = edit(content); err != nil {
			return err
		}
	}
	if err := os.WriteFile(e.configPath, content, 0o600); err != nil {
		return err
	}

	con.note("%s edited", e.configPath)
	return e.reload(ctx, con)
}

// setAlgorithm switches the balancing algorithm.
func (e *environment) setAlgorithm(ctx context.Context, con *console, name string) error {
	return e.applyConfig(ctx, con, func(content []byte) ([]byte, error) {
		return setScalar(content, "algorithm", name)
	})
}

// setRateLimit changes the per-client request limit. Zero disables it.
func (e *environment) setRateLimit(ctx context.Context, con *console, requests int) error {
	return e.applyConfig(ctx, con, func(content []byte) ([]byte, error) {
		return setScalar(content, "rate_limit_per_ip", fmt.Sprint(requests))
	})
}

// backendRequests is the number of requests the backends have answered, read
// from the balancer's own counters.
func (e *environment) backendRequests(ctx context.Context) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.metricsURL(), nil)
	if err != nil {
		return 0, err
	}

	response, err := e.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, err
	}

	total := 0
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "lb_requests_total{") {
			continue
		}
		fields := strings.Fields(line)
		count, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		total += int(count)
	}
	return total, nil
}

// metricsURL is the metrics endpoint beside the status one.
func (e *environment) metricsURL() string {
	return strings.TrimSuffix(e.statusURL, "/status") + "/metrics"
}

// setWeightedWithHeavyBackend switches to weighted balancing and gives one
// backend a heavier share, in a single reload.
func (e *environment) setWeightedWithHeavyBackend(ctx context.Context, con *console, addr string, weight int) error {
	return e.applyConfig(ctx, con,
		func(content []byte) ([]byte, error) {
			return setScalar(content, "algorithm", "weighted_round_robin")
		},
		func(content []byte) ([]byte, error) {
			return setBackendWeight(content, addr, weight)
		})
}

// restore puts the configuration back the way the console found it and brings
// every backend back up.
func (e *environment) restore(ctx context.Context, con *console, baseline []byte, current status) error {
	if err := os.WriteFile(e.configPath, baseline, 0o600); err != nil {
		return err
	}
	con.note("%s restored", e.configPath)

	if err := e.reload(ctx, con); err != nil {
		return err
	}

	up := e.compose(append([]string{"start"}, e.services(current)...)...)
	con.echo(up.String())
	return up.run(ctx)
}
