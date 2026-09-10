package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func TestRoundRobinSpreadsTrafficEvenly(t *testing.T) {
	const requests = 100

	backends := newBackends(t, 10)
	under := start(t, testConfig(backends))

	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Fatalf("%d of %d requests succeeded, want all of them",
			statuses[http.StatusOK], requests)
	}
	for _, b := range backends {
		if got, want := bodies[b.name], requests/len(backends); got != want {
			t.Errorf("%s served %d requests, want %d", b.name, got, want)
		}
	}
}

func TestWeightedRoundRobinFollowsTheConfiguredRatio(t *testing.T) {
	const requests = 120

	backends := newBackends(t, 3)
	cfg := testConfig(backends, 1, 2, 3)
	cfg.Algorithm = config.WeightedRoundRobin

	under := start(t, cfg)
	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Fatalf("%d of %d requests succeeded, want all of them",
			statuses[http.StatusOK], requests)
	}
	// Weights 1, 2 and 3 out of a total of 6.
	for i, want := range []int{requests / 6, requests / 3, requests / 2} {
		if got := bodies[backends[i].name]; got != want {
			t.Errorf("%s served %d requests, want exactly %d", backends[i].name, got, want)
		}
	}
}

func TestLeastConnectionsAvoidsTheSlowBackend(t *testing.T) {
	const requests = 60

	backends := newBackends(t, 3)
	// The first backend takes long enough that requests pile up on it, which is
	// exactly the situation least connections exists for.
	backends[0].slowDown(40 * time.Millisecond)

	cfg := testConfig(backends)
	cfg.Algorithm = config.LeastConnections

	under := start(t, cfg)
	under.sendConcurrently(t, requests)

	slow := backends[0].hits.Load()
	fast := backends[1].hits.Load() + backends[2].hits.Load()
	t.Logf("of %d concurrent requests the slow backend served %d and the two fast ones %d",
		requests, slow, fast)

	if slow == 0 {
		t.Error("the slow backend received nothing, want it kept in rotation")
	}
	if slow >= fast {
		t.Errorf("the slow backend served %d requests against %d for the two fast ones, "+
			"want the load to move away from it", slow, fast)
	}
}
