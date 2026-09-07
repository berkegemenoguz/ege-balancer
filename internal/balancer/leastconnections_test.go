package balancer

import (
	"sync"
	"testing"
)

func TestLeastConnectionsPrefersTheIdleBackend(t *testing.T) {
	backends := pool(3)
	strategy := NewLeastConnections()

	backends[0].Acquire()
	backends[0].Acquire()
	backends[1].Acquire()

	backend, err := strategy.Select(backends)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}
	if backend != backends[2] {
		t.Errorf("selected %s with %d active connections, want the idle backend %s",
			backend.Addr, backend.ActiveConnections(), backends[2].Addr)
	}
}

func TestLeastConnectionsBreaksTiesByOrder(t *testing.T) {
	backends := pool(3)
	strategy := NewLeastConnections()

	backend, err := strategy.Select(backends)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}
	if backend != backends[0] {
		t.Errorf("selected %s, want the first backend when all are equally idle", backend.Addr)
	}
}

func TestLeastConnectionsSpreadsHeldRequests(t *testing.T) {
	const requests = 20

	backends := pool(5)
	strategy := NewLeastConnections()

	// Nothing is released, so every selection has to move on to another backend.
	for range requests {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		backend.Acquire()
	}

	for _, backend := range backends {
		if got, want := backend.ActiveConnections(), int64(requests/len(backends)); got != want {
			t.Errorf("%s holds %d requests, want %d", backend.Addr, got, want)
		}
	}
}

func TestLeastConnectionsFollowsReleases(t *testing.T) {
	backends := pool(2)
	strategy := NewLeastConnections()

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
