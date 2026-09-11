package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

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

	// A backend that drops the connection without answering, not one answering 500.
	backends[1].kill()

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

func TestRetryBudgetStopsARetryStorm(t *testing.T) {
	const requests = 50

	// Every backend fails, slowly enough that the requests overlap. A request may
	// try all four, so without a budget the pool receives four times the client
	// traffic at the moment it can least absorb it.
	storm := func(t *testing.T, budgetPercent float64) (hits int64, metrics string) {
		backends := newBackends(t, 4)
		for _, b := range backends {
			b.fail()
			b.slowDown(100 * time.Millisecond)
		}

		cfg := testConfig(backends)
		cfg.FailurePolicy = config.RetryNextBackend
		cfg.RetryOn5xx = true
		cfg.Retry.MaxRetries = 3
		cfg.Retry.BudgetPercent = budgetPercent
		cfg.Retry.MinRetryConcurrency = 1
		cfg.HealthCheck.UnhealthyThreshold = 1000

		under := start(t, cfg)
		under.sendConcurrently(t, requests)

		for _, b := range backends {
			hits += b.hits.Load()
		}
		return hits, under.scrape(t)
	}

	t.Run("without a limit every request is tried four times", func(t *testing.T) {
		// A request cannot have more retries in flight than itself, so a budget
		// of the whole load never refuses one.
		hits, metrics := storm(t, 100)
		if hits != 4*requests {
			t.Errorf("the backends were reached %d times, want %d", hits, 4*requests)
		}
		if want := "lb_retries_total 150"; !strings.Contains(metrics, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	})

	t.Run("the default share holds retries to a fraction", func(t *testing.T) {
		hits, metrics := storm(t, 20)
		t.Logf("%d client requests reached the failing backends %d times", requests, hits)
		if hits >= 2*requests {
			t.Errorf("the backends were reached %d times, want fewer than %d", hits, 2*requests)
		}
		if want := `lb_rejected_requests_total{reason="retry_budget_exhausted"}`; !strings.Contains(metrics, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	})
}

func TestFailFastSurfacesTheFailure(t *testing.T) {
	const requests = 30

	backends := newBackends(t, 3)
	cfg := testConfig(backends)
	cfg.FailurePolicy = config.FailFast
	cfg.HealthCheck.UnhealthyThreshold = 1000

	backends[1].kill()

	under := start(t, cfg)
	_, statuses := under.send(t, requests)

	if statuses[http.StatusServiceUnavailable] == 0 {
		t.Error("no request failed, want fail_fast to surface the dead backend")
	}
	if statuses[http.StatusOK] == 0 {
		t.Error("no request succeeded, want the healthy backends to keep serving")
	}
}

func TestClientsGet503WhenEveryBackendIsUnreachable(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	for _, b := range backends {
		b.kill()
	}

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

func TestUnhealthyPoolIsStillTried(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	// Both backends answer 500, so health checking empties the pool while they
	// are still reachable. Refusing every request then would turn a degraded
	// service into an outage, so the balancer offers the request anyway and the
	// client sees the backend's own answer.
	for _, b := range backends {
		b.fail()
	}
	eventually(t, func() bool {
		return !strings.Contains(under.scrape(t), `lb_backend_healthy{backend="`+backends[0].addr()+`"} 1`) &&
			!strings.Contains(under.scrape(t), `lb_backend_healthy{backend="`+backends[1].addr()+`"} 1`)
	}, "both backends are taken out of the pool")

	before := backends[0].hits.Load() + backends[1].hits.Load()
	status, body := under.get(t)

	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want the backend's own %d", status, http.StatusInternalServerError)
	}
	if body != "backend is unwell" {
		t.Errorf("body = %q, want the backend's own message", body)
	}
	if backends[0].hits.Load()+backends[1].hits.Load() <= before {
		t.Error("the request was refused instead of being offered to the pool")
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

func TestSuddenBackendLossIsAbsorbed(t *testing.T) {
	const requests = 40

	backends := newBackends(t, 4)
	cfg := testConfig(backends)
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2

	under := start(t, cfg)
	under.send(t, requests)

	// Two of the four vanish mid-flight, without warning.
	backends[1].kill()
	backends[3].kill()

	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Errorf("%d of %d requests succeeded after losing half the pool, want all",
			statuses[http.StatusOK], requests)
	}
	if bodies[backends[0].name]+bodies[backends[2].name] != requests {
		t.Error("the surviving backends did not carry every request")
	}
}

func TestSlowBackendIsBoundedByTheTimeout(t *testing.T) {
	backends := newBackends(t, 2)
	// Far longer than the response timeout below, so the request cannot finish.
	backends[0].slowDown(3 * time.Second)

	cfg := testConfig(backends)
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 1
	cfg.Timeouts.ResponseTimeout = config.Duration(200 * time.Millisecond)

	under := start(t, cfg)

	started := time.Now()
	status, body := under.get(t)
	took := time.Since(started)

	if status != http.StatusOK || body != backends[1].name {
		t.Errorf("answered %d %q, want the fast backend to serve it", status, body)
	}
	if took > 2*time.Second {
		t.Errorf("the request took %s, want the slow backend to be abandoned quickly", took)
	}
}

func TestPoolRecoversAfterEveryBackendFails(t *testing.T) {
	const requests = 20

	backends := newBackends(t, 3)
	under := start(t, testConfig(backends))

	for _, b := range backends {
		b.fail()
	}
	eventually(t, func() bool { return under.status(t).Healthy == 0 },
		"the whole pool is marked unhealthy")

	for _, b := range backends {
		b.heal()
	}
	eventually(t, func() bool { return under.status(t).Healthy == len(backends) },
		"the whole pool recovers")

	bodies, statuses := under.send(t, requests)
	if statuses[http.StatusOK] != requests {
		t.Errorf("%d of %d requests succeeded after recovery, want all",
			statuses[http.StatusOK], requests)
	}
	for _, b := range backends {
		if bodies[b.name] == 0 {
			t.Errorf("%s received no traffic after recovering", b.name)
		}
	}
}

func TestBackendFlappingDoesNotLoseRequests(t *testing.T) {
	backends := newBackends(t, 3)
	cfg := testConfig(backends)
	cfg.FailurePolicy = config.RetryNextBackend
	cfg.Retry.MaxRetries = 2

	under := start(t, cfg)

	// One backend goes up and down repeatedly while traffic flows.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			backends[1].fail()
			time.Sleep(15 * time.Millisecond)
			backends[1].heal()
			time.Sleep(15 * time.Millisecond)
		}
	}()

	var served, failed int
	for range 60 {
		if status, _ := under.get(t); status == http.StatusOK {
			served++
		} else {
			failed++
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done

	// A backend answering 500 is passed through by default, so some requests
	// legitimately carry its error; what must not happen is the balancer
	// failing requests of its own.
	if served == 0 {
		t.Fatal("no request was served while a backend was flapping")
	}
	if failed > 20 {
		t.Errorf("%d of 60 requests failed while one backend of three flapped", failed)
	}
}
