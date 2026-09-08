// Package health implements active (periodic HTTP probe) and passive
// (consecutive failure counter) health checking, and decides when a backend
// leaves or rejoins the pool.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// Checker decides which backends are fit to serve traffic.
//
// Active checking runs on its own schedule once Start is called; passive
// checking is driven by the proxy, which reports the outcome of the requests it
// forwards. Both feed the same consecutive-outcome counters, so a backend that
// fails real traffic is taken out without waiting for the next probe.
type Checker interface {
	// Start begins probing the given backends until ctx is cancelled.
	Start(ctx context.Context, backends []*balancer.Backend)
	// IsHealthy reports whether the backend at addr may receive traffic.
	IsHealthy(addr string) bool
	// ReportSuccess records that a request to addr was served.
	ReportSuccess(addr string)
	// ReportFailure records that a request to addr could not be served.
	ReportFailure(addr string)
}

// state is the health bookkeeping of a single backend.
type state struct {
	healthy bool
	// successes and failures count consecutive outcomes; each one resets the
	// other, so only an unbroken run crosses a threshold.
	successes int
	failures  int
}

// HTTPChecker probes backends over HTTP and tracks their health.
type HTTPChecker struct {
	cfg    config.HealthCheck
	client *http.Client

	// Health is read on every request and written only when a backend changes
	// state, so reads must not contend with each other.
	mu       sync.RWMutex
	backends map[string]*state
}

// New returns a checker that probes cfg.Path on every backend.
func New(cfg config.HealthCheck) *HTTPChecker {
	return &HTTPChecker{
		cfg:      cfg,
		client:   &http.Client{Timeout: time.Duration(cfg.Timeout)},
		backends: make(map[string]*state),
	}
}

// Start registers the backends as healthy and probes each one on its own
// schedule until ctx is cancelled. Backends begin healthy so that traffic flows
// before the first probe completes.
func (c *HTTPChecker) Start(ctx context.Context, backends []*balancer.Backend) {
	c.mu.Lock()
	for _, backend := range backends {
		c.backends[backend.Addr] = &state{healthy: true}
	}
	c.mu.Unlock()

	for _, backend := range backends {
		go c.probeUntilDone(ctx, backend.Addr)
	}
}

// IsHealthy reports whether addr may receive traffic. An address the checker
// does not know is treated as healthy, so a pool without active checking still
// serves.
func (c *HTTPChecker) IsHealthy(addr string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	backend, known := c.backends[addr]
	return !known || backend.healthy
}

// ReportSuccess records a served request, which counts towards recovery.
func (c *HTTPChecker) ReportSuccess(addr string) {
	c.record(addr, true)
}

// ReportFailure records a failed request, which counts towards removal.
func (c *HTTPChecker) ReportFailure(addr string) {
	c.record(addr, false)
}

// probeUntilDone probes addr immediately and then once per interval.
func (c *HTTPChecker) probeUntilDone(ctx context.Context, addr string) {
	ticker := time.NewTicker(time.Duration(c.cfg.Interval))
	defer ticker.Stop()

	for {
		c.probe(ctx, addr)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probe performs one active check and records its outcome.
func (c *HTTPChecker) probe(ctx context.Context, addr string) {
	url := "http://" + addr + c.cfg.Path

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.record(addr, false)
		return
	}

	response, err := c.client.Do(request)
	if err != nil {
		c.record(addr, false)
		return
	}
	defer func() { _ = response.Body.Close() }()

	// Any 2xx answer means the backend is serving.
	c.record(addr, response.StatusCode >= 200 && response.StatusCode < 300)
}

// record applies one outcome to a backend and flips its health once the
// configured threshold is reached.
func (c *HTTPChecker) record(addr string, succeeded bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	backend, known := c.backends[addr]
	if !known {
		return
	}

	if succeeded {
		backend.failures = 0
		backend.successes++
		if !backend.healthy && backend.successes >= c.cfg.HealthyThreshold {
			backend.healthy = true
			slog.Info("backend is healthy again", "backend", addr,
				"successful_checks", backend.successes)
		}
		return
	}

	backend.successes = 0
	backend.failures++
	if backend.healthy && backend.failures >= c.cfg.UnhealthyThreshold {
		backend.healthy = false
		slog.Warn("backend taken out of the pool", "backend", addr,
			"failed_checks", backend.failures)
	}
}
