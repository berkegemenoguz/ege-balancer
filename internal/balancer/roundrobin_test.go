package balancer

import (
	"sync"
	"testing"
)

// pool builds a pool of n backends named after their position.
func pool(n int) []*Backend {
	backends := make([]*Backend, 0, n)
	for i := range n {
		backends = append(backends, NewBackend(string(rune('a'+i))+":5678", 1))
	}
	return backends
}

func TestRoundRobinCyclesInOrder(t *testing.T) {
	backends := pool(3)
	strategy := NewRoundRobin()

	// Two full cycles, so that wrapping back to the first backend is covered.
	for round := range 2 {
		for i, want := range backends {
			got, err := strategy.Select(backends)
			if err != nil {
				t.Fatalf("Select returned an unexpected error: %v", err)
			}
			if got != want {
				t.Fatalf("round %d position %d: got %s, want %s", round, i, got.Addr, want.Addr)
			}
		}
	}
}

func TestRoundRobinDistributesEvenly(t *testing.T) {
	const requests = 1000

	backends := pool(10)
	strategy := NewRoundRobin()

	served := make(map[string]int, len(backends))
	for range requests {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		served[backend.Addr]++
	}

	want := requests / len(backends)
	for _, backend := range backends {
		if got := served[backend.Addr]; got != want {
			t.Errorf("%s served %d requests, want %d", backend.Addr, got, want)
		}
	}
}

func TestRoundRobinWithSingleBackend(t *testing.T) {
	backends := pool(1)
	strategy := NewRoundRobin()

	for range 3 {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		if backend != backends[0] {
			t.Fatalf("got %s, want the only backend", backend.Addr)
		}
	}
}

// TestRoundRobinIsConcurrencySafe fails under -race if the counter is not
// atomic, and fails on the totals if a selection is lost to a data race.
func TestRoundRobinIsConcurrencySafe(t *testing.T) {
	const (
		goroutines         = 50
		requestsPerRoutine = 100
	)

	backends := pool(5)
	strategy := NewRoundRobin()

	counts := make([]map[string]int, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			served := make(map[string]int, len(backends))
			for range requestsPerRoutine {
				backend, err := strategy.Select(backends)
				if err != nil {
					t.Errorf("Select returned an unexpected error: %v", err)
					return
				}
				served[backend.Addr]++
			}
			counts[g] = served
		}()
	}
	wg.Wait()

	total := make(map[string]int, len(backends))
	for _, served := range counts {
		for addr, count := range served {
			total[addr] += count
		}
	}

	want := goroutines * requestsPerRoutine / len(backends)
	for _, backend := range backends {
		if got := total[backend.Addr]; got != want {
			t.Errorf("%s served %d requests, want exactly %d", backend.Addr, got, want)
		}
	}
}
