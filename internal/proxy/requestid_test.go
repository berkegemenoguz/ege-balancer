package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
)

// recordingBackend answers every request and reports the identifier it was sent.
func recordingBackend(t *testing.T, received *string) *balancer.Backend {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*received = r.Header.Get(requestIDHeader)
		_, _ = io.WriteString(w, "backend-1")
	}))
	t.Cleanup(server.Close)

	return &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")}
}

func TestARequestIDIsGeneratedAndReachesBothSides(t *testing.T) {
	var received string
	backends := []*balancer.Backend{recordingBackend(t, &received)}

	response := send(New(testConfig(), balancer.NewRoundRobin(), backends, allHealthy, testMetrics()),
		httptest.NewRequest(http.MethodGet, "/", nil))

	returned := response.Header().Get(requestIDHeader)
	if returned == "" {
		t.Fatal("no identifier was returned to the client")
	}
	if received != returned {
		t.Errorf("the backend was sent %q and the client got %q, want the same identifier",
			received, returned)
	}
}

func TestAClientsOwnRequestIDIsKept(t *testing.T) {
	const sent = "trace-42"

	var received string
	backends := []*balancer.Backend{recordingBackend(t, &received)}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(requestIDHeader, sent)

	response := send(New(testConfig(), balancer.NewRoundRobin(), backends, allHealthy, testMetrics()), request)

	if got := response.Header().Get(requestIDHeader); got != sent {
		t.Errorf("the client got %q, want its own %q", got, sent)
	}
	if received != sent {
		t.Errorf("the backend was sent %q, want the client's own %q", received, sent)
	}
}

func TestAnUnusableRequestIDIsReplaced(t *testing.T) {
	tests := []struct {
		name string
		sent string
	}{
		{"empty", ""},
		{"too long", strings.Repeat("x", maxRequestIDLength+1)},
		{"control characters", "trace\n42"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received string
			backends := []*balancer.Backend{recordingBackend(t, &received)}

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.sent != "" {
				request.Header.Set(requestIDHeader, test.sent)
			}

			response := send(New(testConfig(), balancer.NewRoundRobin(), backends, allHealthy, testMetrics()), request)

			returned := response.Header().Get(requestIDHeader)
			if returned == "" || returned == test.sent {
				t.Errorf("the client got %q, want an identifier of the balancer's own", returned)
			}
			if received != returned {
				t.Errorf("the backend was sent %q, want the returned %q", received, returned)
			}
		})
	}
}

func TestARefusedRequestStillCarriesAnIdentifier(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.RateLimitPerIP = 1

	handler := New(cfg, balancer.NewRoundRobin(), []*balancer.Backend{{Addr: unreachable}},
		allHealthy, testMetrics())

	// The first request spends the only token, so the second is refused before
	// it reaches the proxy core at all.
	send(handler, httptest.NewRequest(http.MethodGet, "/", nil))
	response := send(handler, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	if response.Header().Get(requestIDHeader) == "" {
		t.Error("a refused request carries no identifier, want one to trace it by")
	}
}
