package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/app"
)

// received is what the backend saw of a forwarded request. It is guarded
// because the health checker probes the same handler while the request under
// test is in flight.
type received struct {
	mu sync.Mutex

	method, path, query, body string
	requestID, forwardedFor   string
}

func (r *received) record(request *http.Request, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.method, r.path = request.Method, request.URL.Path
	r.query, r.body = request.URL.RawQuery, body
	r.requestID = request.Header.Get("X-Request-Id")
	r.forwardedFor = request.Header.Get("X-Forwarded-For")
}

func (r *received) read() received {
	r.mu.Lock()
	defer r.mu.Unlock()

	return received{
		method: r.method, path: r.path, query: r.query, body: r.body,
		requestID: r.requestID, forwardedFor: r.forwardedFor,
	}
}

func TestRequestAndResponseSurviveTheProxy(t *testing.T) {
	var seen received

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health probes reach this handler too; they are not the request under
		// test and must not overwrite what it saw.
		if r.URL.Path == "/healthz" {
			return
		}

		body, _ := io.ReadAll(r.Body)
		seen.record(r, string(body))

		w.Header().Set("X-Backend", "echo")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created")
	}))
	defer echo.Close()

	under := start(t, testConfig([]*backend{{
		name:   "echo",
		server: echo,
	}}))

	request, err := http.NewRequest(http.MethodPut,
		under.url+"/orders/42?dry=true", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("building the request failed: %v", err)
	}
	request.Header.Set("X-Request-Id", "abc-123")
	// A client cannot be trusted to describe where it came from.
	request.Header.Set("X-Forwarded-For", "10.0.0.1")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, _ := io.ReadAll(response.Body)
	got := seen.read()

	for _, check := range []struct {
		what      string
		got, want string
	}{
		{"method", got.method, http.MethodPut},
		{"path", got.path, "/orders/42"},
		{"query", got.query, "dry=true"},
		{"body", got.body, "payload"},
		{"forwarded header", got.requestID, "abc-123"},
		{"response body", string(body), "created"},
		{"response header", response.Header.Get("X-Backend"), "echo"},
	} {
		if check.got != check.want {
			t.Errorf("%s = %q, want %q", check.what, check.got, check.want)
		}
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if got.forwardedFor == "10.0.0.1" {
		t.Error("the client's forged X-Forwarded-For reached the backend unchanged")
	}
	if !strings.HasPrefix(got.forwardedFor, "127.0.0.1") {
		t.Errorf("X-Forwarded-For = %q, want the real client address", got.forwardedFor)
	}
}

func TestBackendErrorsReachTheClientUnchanged(t *testing.T) {
	backends := newBackends(t, 1)
	backends[0].fail()

	cfg := testConfig(backends)
	// Keep the backend in the pool so the client sees its own answer.
	cfg.HealthCheck.UnhealthyThreshold = 1000

	under := start(t, cfg)
	status, body := under.get(t)

	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want the backend's own %d", status, http.StatusInternalServerError)
	}
	if body != "backend is unwell" {
		t.Errorf("body = %q, want the backend's own message", body)
	}
}

func TestObservabilityEndpointsReflectTraffic(t *testing.T) {
	const requests = 20

	backends := newBackends(t, 4)
	under := start(t, testConfig(backends))
	under.send(t, requests)

	metrics := under.scrape(t)
	for _, want := range []string{
		`lb_requests_total{backend="` + backends[0].addr() + `",status="200"} 5`,
		`lb_request_duration_seconds_count{backend="` + backends[0].addr() + `"} 5`,
		`lb_backend_healthy{backend="` + backends[0].addr() + `"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}

	response, err := http.Get(under.metricsURL + "/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	var reported struct {
		Algorithm string `json:"algorithm"`
		Healthy   int    `json:"healthy_backends"`
		Total     int    `json:"total_backends"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reported); err != nil {
		t.Fatalf("decoding status failed: %v", err)
	}

	if reported.Algorithm != "round_robin" {
		t.Errorf("algorithm = %q, want the configured one", reported.Algorithm)
	}
	if reported.Healthy != len(backends) || reported.Total != len(backends) {
		t.Errorf("status reports %d of %d healthy, want all %d",
			reported.Healthy, reported.Total, len(backends))
	}
}

func TestShutdownLetsInFlightRequestsFinish(t *testing.T) {
	backends := newBackends(t, 1)
	backends[0].slowDown(300 * time.Millisecond)

	cfg := testConfig(backends)
	path := filepath.Join(t.TempDir(), "lb.yaml")
	writeConfig(t, path, cfg)

	assembled, err := app.New(cfg, path)
	if err != nil {
		t.Fatalf("assembling the balancer failed: %v", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- assembled.Run(ctx, nil) }()

	type result struct {
		status int
		body   string
		err    error
	}
	answered := make(chan result, 1)
	go func() {
		response, err := http.Get("http://" + assembled.Addr())
		if err != nil {
			answered <- result{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		answered <- result{status: response.StatusCode, body: string(body), err: err}
	}()

	// Cancel while the backend is still working on the request.
	time.Sleep(100 * time.Millisecond)
	stop()

	got := <-answered
	if got.err != nil {
		t.Fatalf("the in-flight request failed during shutdown: %v", got.err)
	}
	if got.status != http.StatusOK || got.body != backends[0].name {
		t.Errorf("in-flight request answered %d %q, want %d %q",
			got.status, got.body, http.StatusOK, backends[0].name)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the balancer stopped with %v, want a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the balancer did not shut down within five seconds")
	}
}
