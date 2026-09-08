package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// testMetrics returns a registry the tests can write into and ignore.
func testMetrics() *observability.Metrics {
	return observability.NewMetrics()
}

// allHealthy is a checker that knows no backend, so every address it is asked
// about may serve. Tests that care about health use fakeChecker instead.
var allHealthy = health.New(config.HealthCheck{})

// fakeChecker reports exactly the health the test asks for and records what the
// proxy reported back.
type fakeChecker struct {
	mu        sync.Mutex
	unhealthy map[string]bool
	successes []string
	failures  []string
}

func newFakeChecker(unhealthy ...string) *fakeChecker {
	down := make(map[string]bool, len(unhealthy))
	for _, addr := range unhealthy {
		down[addr] = true
	}
	return &fakeChecker{unhealthy: down}
}

func (f *fakeChecker) Start(context.Context, []*balancer.Backend) {}

func (f *fakeChecker) IsHealthy(addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unhealthy[addr]
}

func (f *fakeChecker) ReportSuccess(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes = append(f.successes, addr)
}

func (f *fakeChecker) ReportFailure(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, addr)
}

func (f *fakeChecker) reported() (successes, failures []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.successes...), append([]string(nil), f.failures...)
}

// testConfig is a fail_fast configuration with generous limits; tests change
// the fields they are about.
func testConfig() *config.Config {
	return &config.Config{
		FailurePolicy: config.FailFast,
		Timeouts:      config.Timeouts{ConnectTimeout: config.Duration(time.Second)},
		Limits:        config.Limits{MaxRequestBodyBytes: 1 << 20},
	}
}

func TestForwardsRequestToBackend(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(body)
		w.Header().Set("X-Backend", "backend-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "handled by backend-1")
	}))
	defer backend.Close()

	response := serveThroughProxy(t, backend,
		httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader("payload")))
	defer func() { _ = response.Body.Close() }()

	if gotMethod != http.MethodPost {
		t.Errorf("backend saw method %q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/orders" {
		t.Errorf("backend saw path %q, want %q", gotPath, "/orders")
	}
	if gotBody != "payload" {
		t.Errorf("backend saw body %q, want %q", gotBody, "payload")
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if got := response.Header.Get("X-Backend"); got != "backend-1" {
		t.Errorf("X-Backend = %q, want the header the backend set", got)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "handled by backend-1" {
		t.Errorf("body = %q, want the backend response", body)
	}
}

func TestOverwritesForwardedForHeader(t *testing.T) {
	var forwardedFor string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		forwardedFor = r.Header.Get("X-Forwarded-For")
	}))
	defer backend.Close()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.7:44444"
	request.Header.Set("X-Forwarded-For", "10.0.0.1")

	response := serveThroughProxy(t, backend, request)
	defer func() { _ = response.Body.Close() }()

	if forwardedFor != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want the real client address, not the forged one",
			forwardedFor)
	}
}

func TestUnreachableBackendReturnsServiceUnavailable(t *testing.T) {
	// Port 1 on the loopback interface is not served by anything.
	handler := newSingleBackend("127.0.0.1:1")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if got := response.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
}

// serveThroughProxy sends request to a proxy pointed at backend and returns the
// response the client would receive.
func serveThroughProxy(t *testing.T, backend *httptest.Server, request *http.Request) *http.Response {
	t.Helper()

	recorder := httptest.NewRecorder()
	newSingleBackend(strings.TrimPrefix(backend.URL, "http://")).ServeHTTP(recorder, request)
	return recorder.Result()
}

// newSingleBackend builds a proxy over a pool holding only addr.
func newSingleBackend(addr string) http.Handler {
	return New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{{Addr: addr}}, allHealthy, testMetrics())
}

func TestDistributesAcrossBackends(t *testing.T) {
	const requests = 9

	served := make([]int, 3)
	backends := make([]*balancer.Backend, 0, len(served))
	for i := range served {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			served[i]++
		}))
		defer server.Close()
		backends = append(backends, &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")})
	}

	handler := New(testConfig(), balancer.NewRoundRobin(), backends, allHealthy, testMetrics())
	for range requests {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
	}

	for i, count := range served {
		if want := requests / len(served); count != want {
			t.Errorf("backend %d served %d requests, want %d", i, count, want)
		}
	}
}

func TestEmptyPoolReturnsServiceUnavailable(t *testing.T) {
	handler := New(testConfig(), balancer.NewRoundRobin(), nil, allHealthy, testMetrics())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
}

func TestUnhealthyBackendIsSkipped(t *testing.T) {
	const requests = 6

	served := make([]int, 2)
	backends := make([]*balancer.Backend, 0, len(served))
	for i := range served {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			served[i]++
		}))
		defer server.Close()
		backends = append(backends, &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")})
	}

	checker := newFakeChecker(backends[0].Addr)
	handler := New(testConfig(), balancer.NewRoundRobin(), backends, checker, testMetrics())

	for range requests {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
	}

	if served[0] != 0 {
		t.Errorf("the unhealthy backend served %d requests, want none", served[0])
	}
	if served[1] != requests {
		t.Errorf("the healthy backend served %d requests, want all %d", served[1], requests)
	}
}

func TestAllBackendsUnhealthyReturnsServiceUnavailable(t *testing.T) {
	backend := &balancer.Backend{Addr: "backend-1:5678"}
	handler := New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{backend},
		newFakeChecker(backend.Addr), testMetrics())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestServedRequestIsReportedAsSuccess(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()

	addr := strings.TrimPrefix(backend.URL, "http://")
	checker := newFakeChecker()
	handler := New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{{Addr: addr}}, checker, testMetrics())

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	successes, failures := checker.reported()
	if len(successes) != 1 || successes[0] != addr {
		t.Errorf("successes = %v, want one report for %s", successes, addr)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %v, want none", failures)
	}
}

func TestUnreachableBackendIsReportedAsFailure(t *testing.T) {
	const addr = "127.0.0.1:1"

	checker := newFakeChecker()
	handler := New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{{Addr: addr}}, checker, testMetrics())

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	successes, failures := checker.reported()
	if len(failures) != 1 || failures[0] != addr {
		t.Errorf("failures = %v, want one report for %s", failures, addr)
	}
	if len(successes) != 0 {
		t.Errorf("successes = %v, want none", successes)
	}
}
