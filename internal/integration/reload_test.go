package integration

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func TestReloadAddsAndRemovesBackends(t *testing.T) {
	const requests = 20

	backends := newBackends(t, 3)
	cfg := testConfig(backends[:2])
	under := start(t, cfg)

	bodies, _ := under.send(t, requests)
	if bodies[backends[2].name] != 0 {
		t.Fatal("a backend that is not configured served traffic")
	}

	// Add the third backend.
	under.reconfigure(t, testConfig(backends))
	eventually(t, func() bool {
		bodies, _ := under.send(t, 6)
		return bodies[backends[2].name] > 0
	}, "the added backend receives traffic")

	// Remove the first one.
	under.reconfigure(t, testConfig(backends[1:]))
	eventually(t, func() bool {
		return !strings.Contains(under.scrape(t),
			`lb_backend_healthy{backend="`+backends[0].addr()+`"}`)
	}, "the removed backend disappears from the metrics")

	before := backends[0].hits.Load()
	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Errorf("%d of %d requests succeeded, want all of them", statuses[http.StatusOK], requests)
	}
	if backends[0].hits.Load() != before {
		t.Error("the removed backend still received traffic")
	}
	if bodies[backends[1].name]+bodies[backends[2].name] != requests {
		t.Error("the remaining backends did not carry every request")
	}
}

func TestReloadSwitchesAlgorithm(t *testing.T) {
	const requests = 60

	backends := newBackends(t, 3)
	cfg := testConfig(backends, 1, 2, 3)
	under := start(t, cfg)

	// Round robin ignores the weights.
	bodies, _ := under.send(t, requests)
	for _, b := range backends {
		if got, want := bodies[b.name], requests/3; got != want {
			t.Errorf("%s served %d requests under round robin, want %d", b.name, got, want)
		}
	}

	weighted := testConfig(backends, 1, 2, 3)
	weighted.Algorithm = config.WeightedRoundRobin
	under.reconfigure(t, weighted)

	bodies, _ = under.send(t, requests)
	for i, want := range []int{requests / 6, requests / 3, requests / 2} {
		if got := bodies[backends[i].name]; got != want {
			t.Errorf("%s served %d requests under weighted round robin, want %d",
				backends[i].name, got, want)
		}
	}

	if !strings.Contains(under.scrape(t), `lb_backend_healthy{`) {
		t.Error("the pool disappeared from the metrics after the reload")
	}
}

func TestReloadAppliesRateLimit(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	if _, statuses := under.send(t, 20); statuses[http.StatusTooManyRequests] > 0 {
		t.Fatal("requests were limited before any limit was configured")
	}

	limited := testConfig(backends)
	limited.Limits.RateLimitPerIP = 3
	under.reconfigure(t, limited)

	if _, statuses := under.send(t, 20); statuses[http.StatusTooManyRequests] == 0 {
		t.Error("the new rate limit was not applied")
	}

	// And it can be lifted again.
	under.reconfigure(t, testConfig(backends))
	if _, statuses := under.send(t, 20); statuses[http.StatusTooManyRequests] > 0 {
		t.Error("the rate limit was not lifted")
	}
}

func TestReloadKeepsServingOnAnInvalidFile(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	if err := os.WriteFile(under.configPath, []byte("algorithm: teleport\n"), 0o600); err != nil {
		t.Fatalf("writing the broken configuration failed: %v", err)
	}
	under.requestReload(t)

	// Give the reload time to be rejected, then confirm nothing changed.
	time.Sleep(50 * time.Millisecond)

	bodies, statuses := under.send(t, 10)
	if statuses[http.StatusOK] != 10 {
		t.Errorf("%d of 10 requests succeeded after a rejected reload, want all", statuses[http.StatusOK])
	}
	if bodies[backends[0].name] == 0 || bodies[backends[1].name] == 0 {
		t.Error("the running configuration was disturbed by an invalid reload")
	}
}

func TestReloadKeepsUnchangeableSettings(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))
	addressBefore := under.url

	moved := testConfig(backends)
	moved.ListenAddr = "127.0.0.1:0"
	moved.Limits.MaxConnections = 7
	moved.FailurePolicy = config.RetryNextBackend
	under.reconfigure(t, moved)

	if under.url != addressBefore {
		t.Error("the listening address changed under a reload")
	}
	// The settings that can be applied still are, so the reload is not refused
	// wholesale because one field needs a restart.
	if _, statuses := under.send(t, 10); statuses[http.StatusOK] != 10 {
		t.Errorf("%d of 10 requests succeeded after the reload, want all", statuses[http.StatusOK])
	}
}

func TestReloadPreservesHealthState(t *testing.T) {
	backends := newBackends(t, 2)
	under := start(t, testConfig(backends))

	backends[0].fail()
	eventually(t, func() bool {
		return strings.Contains(under.scrape(t),
			`lb_backend_healthy{backend="`+backends[0].addr()+`"} 0`)
	}, "the failing backend is taken out of the pool")

	// A reload that does not touch this backend must not hand it a clean slate.
	changed := testConfig(backends)
	changed.RetryOn5xx = true
	under.reconfigure(t, changed)

	if strings.Contains(under.scrape(t),
		`lb_backend_healthy{backend="`+backends[0].addr()+`"} 1`) {
		t.Error("the reload marked a known-unhealthy backend as healthy again")
	}
}
