package balancer

import (
	"math/rand/v2"
	"sync"
	"testing"
)

// seeded returns a least connections strategy whose draws repeat from run to
// run, so that a failing test can be reproduced. It is not safe for concurrent
// use, unlike the strategy the proxy runs.
func seeded() *LeastConnections {
	return &LeastConnections{intn: rand.New(rand.NewPCG(1, 2)).IntN}
}

func TestLeastConnectionsComparesBothBackendsOfAPair(t *testing.T) {
	// With two backends the two draws are the whole pool, so the choice is
	// exactly the least busy one, every time.
	backends := pool(2)
	strategy := seeded()
	backends[0].Acquire()

	for range 100 {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		if backend != backends[1] {
			t.Fatalf("selected %s with %d active connections, want the idle %s",
				backend.Addr, backend.ActiveConnections(), backends[1].Addr)
		}
	}
}

func TestLeastConnectionsNeverChoosesTheBusiest(t *testing.T) {
	// Any pair drawn contains a backend less busy than the busiest one.
	backends := pool(3)
	strategy := seeded()
	backends[0].Acquire()
	backends[0].Acquire()
	backends[1].Acquire()

	for range 1000 {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		if backend == backends[0] {
			t.Fatalf("selected %s, the busiest backend", backend.Addr)
		}
	}
}

func TestLeastConnectionsSpreadsIdleBackendsEvenly(t *testing.T) {
	// All counters equal is the common case when backends answer faster than
	// requests arrive. A scan sent every one of these to the first backend.
	const selections = 10000

	backends := pool(10)
	strategy := seeded()

	chosen := make(map[*Backend]int, len(backends))
	for range selections {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		chosen[backend]++
	}

	share := selections / len(backends)
	for _, backend := range backends {
		if got := chosen[backend]; got < share*8/10 || got > share*12/10 {
			t.Errorf("%s was chosen %d times, want within 20%% of %d", backend.Addr, got, share)
		}
	}
}

func TestLeastConnectionsSpreadsHeldRequests(t *testing.T) {
	const requests = 20

	backends := pool(5)
	strategy := seeded()

	// Nothing is released, so the busier a backend gets the less often it wins.
	for range requests {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		backend.Acquire()
	}

	fair := int64(requests / len(backends))
	for _, backend := range backends {
		if got := backend.ActiveConnections(); got == 0 || got > 2*fair {
			t.Errorf("%s holds %d requests, want between 1 and %d", backend.Addr, got, 2*fair)
		}
	}
}

func TestLeastConnectionsFollowsReleases(t *testing.T) {
	backends := pool(2)
	strategy := seeded()

	backends[0].Acquire()
	backends[1].Acquire()
	backends[1].Acquire()
	backends[1].Release()
	backends[1].Release()

	backend, err := strategy.Select(backends)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}
	if backend != backends[1] {
		t.Errorf("selected %s, want %s after its requests finished", backend.Addr, backends[1].Addr)
	}
}

func TestLeastConnectionsWithOneBackend(t *testing.T) {
	backends := pool(1)
	if backend, err := seeded().Select(backends); err != nil || backend != backends[0] {
		t.Errorf("Select = %v, %v, want the only backend", backend, err)
	}
}

func TestLeastConnectionsIsReproducibleWithASeed(t *testing.T) {
	backends := pool(10)
	first, second := seeded(), seeded()

	for i := range 100 {
		a, _ := first.Select(backends)
		b, _ := second.Select(backends)
		if a != b {
			t.Fatalf("selection %d differs between two strategies with the same seed: %s and %s",
				i, a.Addr, b.Addr)
		}
	}
}

func TestLeastConnectionsOnEmptyPool(t *testing.T) {
	if _, err := NewLeastConnections().Select(nil); err != ErrNoBackends {
		t.Errorf("error = %v, want ErrNoBackends", err)
	}
}

func TestLeastConnectionsName(t *testing.T) {
	if got, want := NewLeastConnections().Name(), "least_connections"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// TestLeastConnectionsIsConcurrencySafe drives the strategy the way the proxy
// does: select, acquire, serve, release.
func TestLeastConnectionsIsConcurrencySafe(t *testing.T) {
	const (
		goroutines         = 50
		requestsPerRoutine = 100
	)

	backends := pool(5)
	strategy := NewLeastConnections()

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requestsPerRoutine {
				backend, err := strategy.Select(backends)
				if err != nil {
					t.Errorf("Select returned an unexpected error: %v", err)
					return
				}
				backend.Acquire()
				backend.Release()
			}
		}()
	}
	wg.Wait()

	for _, backend := range backends {
		if got := backend.ActiveConnections(); got != 0 {
			t.Errorf("%s still holds %d connections, want the counter back at zero",
				backend.Addr, got)
		}
	}
}
