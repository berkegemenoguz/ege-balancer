// Package integration exercises the assembled load balancer end to end: real
// sockets, real HTTP, and the same wiring the binary uses.
package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/app"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// TestMain silences the balancer's own logging, which would otherwise bury the
// test output.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// backend is a mock upstream that answers with its own name and can be made to
// fail or to answer slowly while the test runs.
type backend struct {
	name   string
	server *httptest.Server

	// hits counts client requests; probes counts health checks. Keeping them
	// apart is what lets a test say "no client traffic reached this backend"
	// while the health checker is still polling it.
	hits    atomic.Int64
	probes  atomic.Int64
	failing atomic.Bool
	delay   atomic.Int64
}

// newBackend starts a mock backend named name.
func newBackend(t *testing.T, name string) *backend {
	t.Helper()

	b := &backend{name: name}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			b.probes.Add(1)
		} else {
			b.hits.Add(1)
		}

		if delay := b.delay.Load(); delay > 0 {
			time.Sleep(time.Duration(delay))
		}
		if b.failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "backend is unwell")
			return
		}
		_, _ = io.WriteString(w, b.name)
	}))
	t.Cleanup(b.server.Close)

	return b
}

// addr is the host:port the balancer forwards to.
func (b *backend) addr() string {
	return strings.TrimPrefix(b.server.URL, "http://")
}

// fail makes the backend answer 500 until it is healed.
func (b *backend) fail() { b.failing.Store(true) }

// heal makes the backend answer normally again.
func (b *backend) heal() { b.failing.Store(false) }

// slowDown makes every answer take at least d.
func (b *backend) slowDown(d time.Duration) { b.delay.Store(int64(d)) }

// newBackends starts n mock backends named backend-1 to backend-n.
func newBackends(t *testing.T, n int) []*backend {
	t.Helper()

	backends := make([]*backend, 0, n)
	for i := 1; i <= n; i++ {
		backends = append(backends, newBackend(t, "backend-"+strconv.Itoa(i)))
	}
	return backends
}

// testConfig is a valid configuration pointing at the given backends, with the
// timings shortened so that health transitions happen within a test.
func testConfig(backends []*backend, weights ...int) *config.Config {
	configured := make([]config.Backend, 0, len(backends))
	for i, b := range backends {
		weight := 1
		if i < len(weights) {
			weight = weights[i]
		}
		configured = append(configured, config.Backend{Addr: b.addr(), Weight: weight})
	}

	return &config.Config{
		ListenAddr:     "127.0.0.1:0",
		MetricsAddr:    "127.0.0.1:0",
		Algorithm:      config.RoundRobin,
		FailurePolicy:  config.FailFast,
		Retry:          config.Retry{MaxRetries: 2},
		CircuitBreaker: config.CircuitBreaker{FailureThreshold: 5, OpenDuration: config.Duration(time.Second)},
		Backends:       configured,
		HealthCheck: config.HealthCheck{
			Path:               "/healthz",
			Interval:           config.Duration(10 * time.Millisecond),
			Timeout:            config.Duration(5 * time.Millisecond),
			HealthyThreshold:   2,
			UnhealthyThreshold: 2,
		},
		Timeouts: config.Timeouts{
			ConnectTimeout: config.Duration(time.Second),
			// Kept short on purpose: a client opening more connections than it
			// uses leaves some that never send a request, and shutdown waits
			// for those until their read timeout expires.
			ReadTimeout:  config.Duration(500 * time.Millisecond),
			WriteTimeout: config.Duration(2 * time.Second),
			IdleTimeout:  config.Duration(500 * time.Millisecond),
		},
		Limits: config.Limits{
			MaxConnections:      100,
			MaxRequestBodyBytes: 1 << 20,
		},
		Logging: config.Logging{Level: config.LevelError, Format: config.FormatJSON},
	}
}

// balancer is a running load balancer under test.
type balancerUnderTest struct {
	url        string
	metricsURL string
	stop       context.CancelFunc
	done       <-chan error
}

// start assembles and runs the balancer described by cfg, and stops it when the
// test ends.
func start(t *testing.T, cfg *config.Config) *balancerUnderTest {
	t.Helper()

	assembled, err := app.New(cfg)
	if err != nil {
		t.Fatalf("assembling the balancer failed: %v", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- assembled.Run(ctx) }()

	under := &balancerUnderTest{
		url:        "http://" + assembled.Addr(),
		metricsURL: "http://" + assembled.MetricsAddr(),
		stop:       stop,
		done:       done,
	}
	t.Cleanup(func() { under.shutdown(t) })

	return under
}

// shutdown stops the balancer and reports an unclean exit.
func (b *balancerUnderTest) shutdown(t *testing.T) {
	t.Helper()

	b.stop()
	select {
	case err := <-b.done:
		if err != nil {
			t.Errorf("the balancer stopped with %v, want a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the balancer did not shut down within five seconds")
	}
}

// get sends one request and returns its status and body.
func (b *balancerUnderTest) get(t *testing.T) (int, string) {
	t.Helper()

	response, err := http.Get(b.url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body failed: %v", err)
	}
	return response.StatusCode, strings.TrimSpace(string(body))
}

// send issues n sequential requests and counts the bodies and statuses seen.
func (b *balancerUnderTest) send(t *testing.T, n int) (bodies map[string]int, statuses map[int]int) {
	t.Helper()

	bodies, statuses = make(map[string]int), make(map[int]int)
	for range n {
		status, body := b.get(t)
		bodies[body]++
		statuses[status]++
	}
	return bodies, statuses
}

// sendConcurrently issues n requests at once and waits for all of them.
func (b *balancerUnderTest) sendConcurrently(t *testing.T, n int) {
	t.Helper()

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := http.Get(b.url)
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// scrape reads the balancer's own metrics endpoint.
func (b *balancerUnderTest) scrape(t *testing.T) string {
	t.Helper()

	response, err := http.Get(b.metricsURL + "/metrics")
	if err != nil {
		t.Fatalf("scraping metrics failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading metrics failed: %v", err)
	}
	return string(body)
}

// eventually waits up to two seconds for condition to hold.
func eventually(t *testing.T, condition func() bool, describe string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", describe)
}
