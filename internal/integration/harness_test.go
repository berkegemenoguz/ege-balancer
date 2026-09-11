// Package integration exercises the assembled load balancer end to end: real
// sockets, real HTTP, and the same wiring the binary uses.
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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

	// dead makes the backend drop every connection without answering, as a
	// crashed process would, while keeping its port. Closing the server instead
	// would free the port, and whatever listener is handed it next — the
	// balancer's own included — would answer in the dead backend's place.
	dead atomic.Bool
}

// newBackend starts a mock backend named name.
func newBackend(t *testing.T, name string) *backend {
	t.Helper()

	b := &backend{name: name}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.dead.Load() {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
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

// kill makes the backend drop every connection from now on, without giving up
// its port.
func (b *backend) kill() { b.dead.Store(true) }

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
			ConnectTimeout:  config.Duration(time.Second),
			ResponseTimeout: config.Duration(2 * time.Second),
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
	configPath string
	reload     chan struct{}
	stop       context.CancelFunc
	done       <-chan error
}

// writeConfig renders cfg as YAML at path, the way an operator would edit it.
func writeConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()

	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("encoding the configuration failed: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("writing the configuration failed: %v", err)
	}
}

// start writes cfg to a file, assembles the balancer from it, and runs it until
// the test ends. Going through a file is what the binary does, and it is what
// lets a test reload.
func start(t *testing.T, cfg *config.Config) *balancerUnderTest {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lb.yaml")
	writeConfig(t, path, cfg)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the test configuration is not valid: %v", err)
	}

	assembled, err := app.New(loaded, path)
	if err != nil {
		t.Fatalf("assembling the balancer failed: %v", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	reload := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- assembled.Run(ctx, reload) }()

	under := &balancerUnderTest{
		url:        "http://" + assembled.Addr(),
		metricsURL: "http://" + assembled.MetricsAddr(),
		configPath: path,
		reload:     reload,
		stop:       stop,
		done:       done,
	}
	t.Cleanup(func() { under.shutdown(t) })

	return under
}

// reconfigure rewrites the configuration file, asks the balancer to reload it
// the way sending SIGHUP would, and waits until the new configuration is the
// one being served.
func (b *balancerUnderTest) reconfigure(t *testing.T, cfg *config.Config) {
	t.Helper()

	applied := b.status(t).Reloads
	writeConfig(t, b.configPath, cfg)
	b.requestReload(t)

	eventually(t, func() bool { return b.status(t).Reloads > applied },
		"the new configuration is applied")
}

// requestReload asks for a reload of whatever the configuration file now holds.
func (b *balancerUnderTest) requestReload(t *testing.T) {
	t.Helper()

	select {
	case b.reload <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("the balancer did not accept a reload request")
	}
}

// reportedStatus is the part of /status the tests read.
type reportedStatus struct {
	Algorithm string `json:"algorithm"`
	Reloads   int64  `json:"reloads"`
	Healthy   int    `json:"healthy_backends"`
	Total     int    `json:"total_backends"`
}

// status reads the balancer's own view of itself.
func (b *balancerUnderTest) status(t *testing.T) reportedStatus {
	t.Helper()

	response, err := http.Get(b.metricsURL + "/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	var reported reportedStatus
	if err := json.NewDecoder(response.Body).Decode(&reported); err != nil {
		t.Fatalf("decoding status failed: %v", err)
	}
	return reported
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
