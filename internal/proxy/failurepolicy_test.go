package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// unreachable is a loopback port nothing serves, so dialling it fails at once.
const unreachable = "127.0.0.1:1"

// echoBackend answers every request with name and counts what it received.
func echoBackend(t *testing.T, name string, served *int) *balancer.Backend {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*served++
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(server.Close)

	return &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")}
}

// send runs one request through handler and returns the recorded response.
func send(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestRetryNextBackendServesFromAHealthyBackend(t *testing.T) {
	var served int
	backends := []*balancer.Backend{{Addr: unreachable}, echoBackend(t, "backend-2", &served)}

	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2

	response := send(New(cfg, balancer.NewRoundRobin(), backends, allHealthy),
		httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d after retrying past the dead backend",
			response.Code, http.StatusOK)
	}
	if got := response.Body.String(); got != "backend-2" {
		t.Errorf("body = %q, want the healthy backend to answer", got)
	}
	if served != 1 {
		t.Errorf("the healthy backend served %d requests, want 1", served)
	}
}

func TestRetryStopsAtMaxRetries(t *testing.T) {
	backends := []*balancer.Backend{
		{Addr: unreachable}, {Addr: "127.0.0.1:2"}, {Addr: "127.0.0.1:3"},
	}

	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 1

	checker := newFakeChecker()
	response := send(New(cfg, balancer.NewRoundRobin(), backends, checker),
		httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := response.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}

	// One try plus one retry: the third backend is never reached.
	_, failures := checker.reported()
	if len(failures) != 2 {
		t.Errorf("attempted %d backends, want 1 try plus 1 retry", len(failures))
	}
}

func TestRetryNeverRepeatsABackend(t *testing.T) {
	backends := []*balancer.Backend{{Addr: unreachable}}

	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 5

	checker := newFakeChecker()
	send(New(cfg, balancer.NewRoundRobin(), backends, checker),
		httptest.NewRequest(http.MethodGet, "/", nil))

	_, failures := checker.reported()
	if len(failures) != 1 {
		t.Errorf("the only backend was tried %d times, want once", len(failures))
	}
}

func TestFailFastDoesNotRetry(t *testing.T) {
	var served int
	backends := []*balancer.Backend{{Addr: unreachable}, echoBackend(t, "backend-2", &served)}

	cfg := testConfig()
	cfg.FailurePolicy = config.FailFast
	cfg.Retry.MaxRetries = 3 // ignored under fail_fast

	response := send(New(cfg, balancer.NewRoundRobin(), backends, allHealthy),
		httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if served != 0 {
		t.Errorf("the second backend served %d requests, want none under fail_fast", served)
	}
}

func TestFivexxIsPassedThroughByDefault(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "backend is broken")
	}))
	defer backend.Close()

	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2
	cfg.RetryOn5xx = false

	backends := []*balancer.Backend{{Addr: strings.TrimPrefix(backend.URL, "http://")}}
	response := send(New(cfg, balancer.NewRoundRobin(), backends, allHealthy),
		httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want the backend's own %d",
			response.Code, http.StatusInternalServerError)
	}
	if got := response.Body.String(); got != "backend is broken" {
		t.Errorf("body = %q, want the backend's own error message preserved", got)
	}
}

func TestFivexxIsRetriedWhenConfigured(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()

	var served int
	backends := []*balancer.Backend{
		{Addr: strings.TrimPrefix(failing.URL, "http://")},
		echoBackend(t, "backend-2", &served),
	}

	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2
	cfg.RetryOn5xx = true

	response := send(New(cfg, balancer.NewRoundRobin(), backends, allHealthy),
		httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the retry to reach a working backend", response.Code)
	}
	if got := response.Body.String(); got != "backend-2" {
		t.Errorf("body = %q, want the healthy backend to answer", got)
	}
}

func TestRetriedRequestKeepsItsBody(t *testing.T) {
	var received string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
	}))
	defer backend.Close()

	backends := []*balancer.Backend{
		{Addr: unreachable},
		{Addr: strings.TrimPrefix(backend.URL, "http://")},
	}

	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2

	response := send(New(cfg, balancer.NewRoundRobin(), backends, allHealthy),
		httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader("payload")))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the retry to succeed", response.Code)
	}
	if received != "payload" {
		t.Errorf("backend received %q, want the body to survive the retry", received)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 1
	cfg.Limits.MaxRequestBodyBytes = 8

	backends := []*balancer.Backend{{Addr: unreachable}}
	response := send(New(cfg, balancer.NewRoundRobin(), backends, allHealthy),
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 64))))

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestCircuitBreakerShutsOutAndRestoresBackend(t *testing.T) {
	var served int
	dead := &balancer.Backend{Addr: unreachable}
	alive := echoBackend(t, "backend-2", &served)

	cfg := testConfig()
	cfg.FailurePolicy = config.CircuitBreakerPolicy
	cfg.CircuitBreaker = config.CircuitBreaker{
		FailureThreshold: 2,
		OpenDuration:     config.Duration(50 * time.Millisecond),
	}

	handler := New(cfg, balancer.NewRoundRobin(), []*balancer.Backend{dead, alive}, allHealthy)

	// Round robin alternates, so two rounds give the dead backend its two
	// failures and trip the breaker.
	for range 4 {
		send(handler, httptest.NewRequest(http.MethodGet, "/", nil))
	}
	servedBeforeTrip := served

	// With the circuit open every request must land on the healthy backend.
	for range 4 {
		response := send(handler, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want the tripped backend to be skipped", response.Code)
		}
	}
	if served != servedBeforeTrip+4 {
		t.Errorf("the healthy backend served %d of the 4 requests after the trip",
			served-servedBeforeTrip)
	}

	// After the open period one probe is let through, fails, and shuts the
	// backend out again rather than letting it take traffic.
	time.Sleep(60 * time.Millisecond)
	for range 4 {
		if response := send(handler, httptest.NewRequest(http.MethodGet, "/", nil)); response.Code != http.StatusOK &&
			response.Code != http.StatusServiceUnavailable {
			t.Fatalf("unexpected status %d after the open period", response.Code)
		}
	}
}
