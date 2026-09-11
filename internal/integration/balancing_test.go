package integration

import (
	"io"
	"net/http"
	"sync"
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

func TestLeastConnectionsSpreadsSequentialTraffic(t *testing.T) {
	const requests = 100

	// Each request finishes before the next arrives, so every selection sees
	// every backend idle. Choosing the least busy by scanning sent all of these
	// to the first backend.
	backends := newBackends(t, 10)
	cfg := testConfig(backends)
	cfg.Algorithm = config.LeastConnections

	under := start(t, cfg)
	bodies, statuses := under.send(t, requests)

	if statuses[http.StatusOK] != requests {
		t.Fatalf("%d of %d requests succeeded, want all of them",
			statuses[http.StatusOK], requests)
	}
	t.Logf("%d sequential requests over %d backends: %v", requests, len(backends), bodies)
	for _, b := range backends {
		if got := bodies[b.name]; got > 3*requests/len(backends) {
			t.Errorf("%s served %d of %d requests, want the idle pool shared", b.name, got, requests)
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

func TestLeastConnectionsAvoidsASlowBackendUnderSustainedLoad(t *testing.T) {
	const workers, perWorker = 10, 20

	// Unlike a single burst, sustained traffic lets requests build up on the
	// slow backend, and from then on any pair that includes it is won by the
	// other backend.
	backends := newBackends(t, 3)
	backends[0].slowDown(40 * time.Millisecond)

	cfg := testConfig(backends)
	cfg.Algorithm = config.LeastConnections
	under := start(t, cfg)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
				response, err := http.Get(under.url)
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		})
	}
	wg.Wait()

	total := int64(workers * perWorker)
	slow := backends[0].hits.Load()
	t.Logf("of %d requests from %d workers the slow backend served %d", total, workers, slow)

	if slow == 0 {
		t.Error("the slow backend received nothing, want it kept in rotation")
	}
	if slow >= total/10 {
		t.Errorf("the slow backend served %d of %d requests, want under a tenth", slow, total)
	}
}
