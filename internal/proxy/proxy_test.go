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

// testTimeouts are short enough to keep a failing dial from slowing the suite.
var testTimeouts = config.Timeouts{ConnectTimeout: config.Duration(time.Second)}

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
	return New(balancer.NewRoundRobin(), []*balancer.Backend{{Addr: addr}}, testTimeouts)
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

	handler := New(balancer.NewRoundRobin(), backends, testTimeouts)
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
	handler := New(balancer.NewRoundRobin(), nil, testTimeouts)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
}
