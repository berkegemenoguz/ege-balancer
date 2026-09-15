package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// probe requests one of the balancer's own health endpoints.
func (b *balancerUnderTest) probe(t *testing.T, path string) (int, string) {
	t.Helper()

	response, err := http.Get(b.metricsURL + path)
	if err != nil {
		t.Fatalf("%s request failed: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading %s failed: %v", path, err)
	}
	return response.StatusCode, strings.TrimSpace(string(body))
}

func TestReadinessFollowsTheHealthOfThePool(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	if code, _ := under.probe(t, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz answered %d with every backend up, want %d", code, http.StatusOK)
	}

	for _, b := range backends {
		b.kill()
	}
	eventually(t, func() bool {
		code, body := under.probe(t, "/readyz")
		return code == http.StatusServiceUnavailable && body == "no healthy backend"
	}, "/readyz reports that no backend is healthy")

	// The balancer itself is fine; restarting it would bring no backend back.
	if code, _ := under.probe(t, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz answered %d with every backend down, want %d", code, http.StatusOK)
	}

	backends[0].dead.Store(false)
	eventually(t, func() bool {
		code, _ := under.probe(t, "/readyz")
		return code == http.StatusOK
	}, "/readyz recovers once a backend does")
}

func TestReadinessFailsWhileInFlightRequestsDrain(t *testing.T) {
	backends := newBackends(t, 1)
	backends[0].slowDown(500 * time.Millisecond)
	under := start(t, testConfig(backends))

	answered := make(chan int, 1)
	go func() {
		response, err := http.Get(under.url)
		if err != nil {
			answered <- 0
			return
		}
		_ = response.Body.Close()
		answered <- response.StatusCode
	}()

	// Stop while the backend is still working on the request.
	time.Sleep(100 * time.Millisecond)
	under.stop()

	eventually(t, func() bool {
		code, body := under.probe(t, "/readyz")
		return code == http.StatusServiceUnavailable && body == "shutting down"
	}, "/readyz reports the shutdown while the request drains")

	if code := <-answered; code != http.StatusOK {
		t.Errorf("the in-flight request answered %d, want %d", code, http.StatusOK)
	}
}
