package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// abortingBackend promises an answer, sends half of it and drops the
// connection, as a backend that crashes while answering does.
func abortingBackend(t *testing.T) *balancer.Backend {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "8192")
		_, _ = w.Write(make([]byte, 4096))
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)

	return &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")}
}

// fetch sends one request through handler over a real socket, which is where
// an aborted answer shows, and returns the error the client saw.
func fetch(t *testing.T, handler http.Handler) error {
	t.Helper()

	front := httptest.NewServer(handler)
	defer front.Close()

	response, err := http.Get(front.URL)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, err = io.ReadAll(response.Body)
	return err
}

func TestAnAbortedRetryGivesBackItsBudget(t *testing.T) {
	cfg := testConfig()
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2

	// The first attempt fails to connect, so the answer that is broken off
	// belongs to a retry.
	backends := []*balancer.Backend{{Addr: unreachable}, abortingBackend(t)}
	handler := New(cfg, balancer.NewRoundRobin(), backends, allHealthy, testMetrics())

	if err := fetch(t, handler); err == nil {
		t.Fatal("the client read a whole answer, want it broken off")
	}

	budget := handler.core.current.Load().budget
	if held := budget.retries.Load(); held != 0 {
		t.Errorf("%d retries still held after the request ended, want the budget given back", held)
	}
}

func TestABackendThatBreaksOffItsAnswerIsCountedAgainstIt(t *testing.T) {
	backend := abortingBackend(t)
	checker := newFakeChecker()
	metrics := observability.NewMetrics()

	if err := fetch(t, New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{backend}, checker, metrics)); err == nil {
		t.Fatal("the client read a whole answer, want it broken off")
	}

	if _, failures := checker.reported(); len(failures) != 1 || failures[0] != backend.Addr {
		t.Errorf("the health checker heard of failures %v, want one for %s", failures, backend.Addr)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `lb_backend_failures_total{backend="` + backend.Addr + `"} 1`; !strings.Contains(recorder.Body.String(), want) {
		t.Errorf("metrics do not contain %q", want)
	}
}

func TestAClientThatHangsUpIsNotCountedAgainstTheBackend(t *testing.T) {
	// The backend starts a long answer and keeps it open until the balancer
	// gives up on it. It sends more than the balancer buffers before it waits,
	// so that the start of the answer reaches the client.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		_, _ = w.Write(make([]byte, 64<<10))
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	backend := &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")}
	checker := newFakeChecker()
	front := httptest.NewServer(New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{backend}, checker, testMetrics()))
	defer front.Close()

	response, err := http.Get(front.URL)
	if err != nil {
		t.Fatalf("request failed before the answer began: %v", err)
	}
	_, _ = io.ReadFull(response.Body, make([]byte, 512))
	_ = response.Body.Close() // the client hangs up part way through

	deadline := time.Now().Add(2 * time.Second)
	for backend.ActiveConnections() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if backend.ActiveConnections() > 0 {
		t.Fatal("the balancer never finished the attempt")
	}
	if _, failures := checker.reported(); len(failures) != 0 {
		t.Errorf("the health checker heard of failures %v, want none: the client left, the backend did nothing wrong", failures)
	}
}

func TestAClientThatLeavesBeforeTheAnswerIsNotCountedAgainstAnyBackend(t *testing.T) {
	// Each backend takes longer to answer than the client is willing to wait.
	var received atomic.Int64
	backends := make([]*balancer.Backend, 0, 3)
	for range 3 {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			received.Add(1)
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		t.Cleanup(server.Close)
		backends = append(backends, &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")})
	}

	checker := newFakeChecker()
	metrics := observability.NewMetrics()
	front := httptest.NewServer(New(retryingConfig(), balancer.NewRoundRobin(), backends, checker, metrics))
	defer front.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := http.DefaultClient.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("the client was answered, want it to give up first")
	}

	deadline := time.Now().Add(2 * time.Second)
	for busy(backends) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if busy(backends) {
		t.Fatal("the balancer never finished the request")
	}

	if n := received.Load(); n != 1 {
		t.Errorf("%d backends were offered the request, want 1: a retry for a client that has left reaches no one", n)
	}
	if _, failures := checker.reported(); len(failures) != 0 {
		t.Errorf("the health checker heard of failures %v, want none: the client left, no backend did anything wrong", failures)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, counted := range []string{"lb_backend_failures_total{", "lb_rejected_requests_total{"} {
		if strings.Contains(recorder.Body.String(), counted) {
			t.Errorf("metrics count %s, want nothing: the client left", strings.TrimSuffix(counted, "{"))
		}
	}
}

// busy reports whether any backend still has a request in flight.
func busy(backends []*balancer.Backend) bool {
	for _, backend := range backends {
		if backend.ActiveConnections() > 0 {
			return true
		}
	}
	return false
}
