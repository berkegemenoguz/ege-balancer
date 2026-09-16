package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// post sends one request with a body and returns its status.
func (b *balancerUnderTest) post(t *testing.T) int {
	t.Helper()

	response, err := http.Post(b.url+"/orders", "text/plain", strings.NewReader("one order"))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode
}

func TestAPostIsNotRetriedOnceABackendHasIt(t *testing.T) {
	backends := newBackends(t, 2)

	cfg := testConfig(backends)
	cfg.FailurePolicy = config.RetryNextBackend
	// Keep the failing backend in the pool, so the retry decision rather than
	// health checking is what the test measures.
	cfg.HealthCheck.UnhealthyThreshold = 1000

	under := start(t, cfg)

	// The backend now takes the request and drops the connection without
	// answering, as a backend that carried it out and then died would.
	backends[0].kill()

	const requests = 6
	refused := 0
	for range requests {
		if under.post(t) == http.StatusServiceUnavailable {
			refused++
		}
	}

	// Round robin sends half the requests to the dead backend; those are the
	// ones that must not be sent on to another backend.
	if refused == 0 {
		t.Error("every POST was answered, want the ones the dead backend took refused")
	}
	if hits := backends[1].hits.Load(); int(hits)+refused != requests {
		t.Errorf("the live backend served %d and %d were refused, want %d in total",
			hits, refused, requests)
	}
}

func TestAGetIsStillRetriedPastTheSameFailure(t *testing.T) {
	backends := newBackends(t, 2)

	cfg := testConfig(backends)
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.HealthCheck.UnhealthyThreshold = 1000

	under := start(t, cfg)
	backends[0].kill()

	_, statuses := under.send(t, 6)
	if statuses[http.StatusOK] != 6 {
		t.Errorf("statuses = %v, want every idempotent request retried to the live backend", statuses)
	}
}
