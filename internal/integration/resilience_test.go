package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func TestFailingBackendLeavesAndRejoinsThePool(t *testing.T) {
	const requests = 27

	backends := newBackends(t, 3)
	under := start(t, testConfig(backends))

	// The health checker must notice on its own, without any client traffic.
	backends[1].fail()
	eventually(t, func() bool {
		return strings.Contains(under.scrape(t),
			`lb_backend_healthy{backend="`+backends[1].addr()+`"} 0`)
	}, "the failing backend is taken out of the pool")

	before := backends[1].probes.Load()
	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Errorf("%d of %d requests succeeded, want all of them despite the failing backend",
			statuses[http.StatusOK], requests)
	}
	if bodies[backends[1].name] != 0 {
		t.Errorf("the failing backend answered %d requests, want none",
			bodies[backends[1].name])
	}
	// Health probes still reach it; client traffic must not.
	if got := bodies["backend is unwell"]; got != 0 {
		t.Errorf("%d clients saw the failing backend's error, want none", got)
	}

	// The two healthy backends carry everything between them.
	for _, i := range []int{0, 2} {
		if got, want := bodies[backends[i].name], requests/2; got < want {
			t.Errorf("%s served %d requests, want at least %d", backends[i].name, got, want)
		}
	}

	backends[1].heal()
	eventually(t, func() bool {
		return strings.Contains(under.scrape(t),
			`lb_backend_healthy{backend="`+backends[1].addr()+`"} 1`)
	}, "the recovered backend rejoins the pool")

	if backends[1].probes.Load() <= before {
		t.Error("the recovered backend received no probes")
	}

	bodies, _ = under.send(t, requests)
	if bodies[backends[1].name] == 0 {
		t.Error("the recovered backend received no client traffic")
	}
}

func TestRetryHidesADeadBackendFromClients(t *testing.T) {
	const requests = 30

	backends := newBackends(t, 3)
	cfg := testConfig(backends)
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2
	// Long enough that the health checker cannot mask the retry behaviour.
	cfg.HealthCheck.UnhealthyThreshold = 1000

	// A backend that refuses connections outright, not one answering 500.
	backends[1].server.Close()

	under := start(t, cfg)
	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Errorf("%d of %d requests succeeded, want retry to cover every one",
			statuses[http.StatusOK], requests)
	}
	if bodies[backends[1].name] != 0 {
		t.Error("the dead backend answered a request")
	}
}

func TestFailFastSurfacesTheFailure(t *testing.T) {
	const requests = 30

	backends := newBackends(t, 3)
	cfg := testConfig(backends)
	cfg.FailurePolicy = config.FailFast
	cfg.HealthCheck.UnhealthyThreshold = 1000

	backends[1].server.Close()

	under := start(t, cfg)
	_, statuses := under.send(t, requests)

	if statuses[http.StatusServiceUnavailable] == 0 {
		t.Error("no request failed, want fail_fast to surface the dead backend")
	}
	if statuses[http.StatusOK] == 0 {
		t.Error("no request succeeded, want the healthy backends to keep serving")
	}
}

func TestClientsGet503WhenEveryBackendIsDown(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	for _, b := range backends {
		b.fail()
	}
	eventually(t, func() bool {
		return strings.Count(under.scrape(t), `lb_backend_healthy{`) == 2 &&
			!strings.Contains(under.scrape(t), `lb_backend_healthy{backend="`+backends[0].addr()+`"} 1`) &&
			!strings.Contains(under.scrape(t), `lb_backend_healthy{backend="`+backends[1].addr()+`"} 1`)
	}, "both backends are taken out of the pool")

	response, err := http.Get(under.url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if got := response.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
}

func TestRateLimitProtectsTheBackends(t *testing.T) {
	const (
		limit    = 5
		requests = 20
	)

	backends := newBackends(t, 2)
	cfg := testConfig(backends)
	cfg.Limits.RateLimitPerIP = limit

	under := start(t, cfg)
	_, statuses := under.send(t, requests)

	if statuses[http.StatusTooManyRequests] == 0 {
		t.Fatal("no request was rate limited")
	}
	if statuses[http.StatusOK] > limit+1 {
		t.Errorf("%d requests got through, want no more than the burst of %d",
			statuses[http.StatusOK], limit)
	}

	served := backends[0].hits.Load() + backends[1].hits.Load()
	if served > int64(limit+1) {
		t.Errorf("the backends served %d requests, want the limiter to stop the rest", served)
	}
}
