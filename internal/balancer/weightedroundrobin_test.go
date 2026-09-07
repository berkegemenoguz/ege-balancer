package balancer

import (
	"strings"
	"sync"
	"testing"
)

// weighted builds a pool whose backends are named a, b, c ... with the given
// weights.
func weighted(weights ...int) []*Backend {
	backends := make([]*Backend, 0, len(weights))
	for i, weight := range weights {
		backends = append(backends, &Backend{Addr: string(rune('a' + i)), Weight: weight})
	}
	return backends
}

// sequence records the addresses chosen for the next n requests.
func sequence(t *testing.T, strategy LBStrategy, backends []*Backend, n int) string {
	t.Helper()

	var chosen strings.Builder
	for range n {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		chosen.WriteString(backend.Addr)
	}
	return chosen.String()
}

func TestWeightedRoundRobinSpreadsTheHeavyBackend(t *testing.T) {
	backends := weighted(5, 1, 1)

	// The smooth algorithm interleaves the heavy backend instead of serving
	// five requests from it in a row.
	if got, want := sequence(t, NewWeightedRoundRobin(), backends, 7), "aabacaa"; got != want {
		t.Errorf("sequence = %q, want %q", got, want)
	}
}

func TestWeightedRoundRobinRepeatsItsCycle(t *testing.T) {
	backends := weighted(3, 1)
	strategy := NewWeightedRoundRobin()

	first := sequence(t, strategy, backends, 4)
	second := sequence(t, strategy, backends, 4)
	if first != second {
		t.Errorf("second cycle = %q, want it to repeat the first cycle %q", second, first)
	}
}

func TestWeightedRoundRobinMatchesTheConfiguredRatio(t *testing.T) {
	const requests = 1200

	backends := weighted(1, 2, 3)
	strategy := NewWeightedRoundRobin()

	served := make(map[string]int, len(backends))
	for range requests {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		served[backend.Addr]++
	}

	// Weights 1, 2 and 3 out of a total of 6 mean 200, 400 and 600 requests.
	for _, backend := range backends {
		want := requests * backend.Weight / 6
		if got := served[backend.Addr]; got != want {
			t.Errorf("%s served %d requests, want exactly %d", backend.Addr, got, want)
		}
	}
}

func TestWeightedRoundRobinTreatsEqualWeightsAsRoundRobin(t *testing.T) {
	backends := weighted(1, 1, 1)

	if got, want := sequence(t, NewWeightedRoundRobin(), backends, 6), "abcabc"; got != want {
		t.Errorf("sequence = %q, want %q", got, want)
	}
}

func TestWeightedRoundRobinTreatsMissingWeightAsOne(t *testing.T) {
	backends := weighted(0, 2)

	// A pool built without weights must still make progress on both backends.
	got := sequence(t, NewWeightedRoundRobin(), backends, 6)
	if strings.Count(got, "a") != 2 || strings.Count(got, "b") != 4 {
		t.Errorf("sequence = %q, want the zero weight treated as 1 against a weight of 2", got)
	}
}

func TestWeightedRoundRobinIgnoresBackendsOutsideThePool(t *testing.T) {
	backends := weighted(1, 1, 1)
	strategy := NewWeightedRoundRobin()

	// Day 6 removes unhealthy backends from the pool it passes in, so the
	// strategy must keep working on a shrinking slice.
	if got, want := sequence(t, strategy, backends, 3), "abc"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	for range 4 {
		backend, err := strategy.Select(backends[:2])
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		if backend.Addr == "c" {
			t.Fatal("selected a backend that is no longer in the pool")
		}
	}
}

func TestWeightedRoundRobinOnEmptyPool(t *testing.T) {
	if _, err := NewWeightedRoundRobin().Select(nil); err != ErrNoBackends {
		t.Errorf("error = %v, want ErrNoBackends", err)
	}
}

func TestWeightedRoundRobinName(t *testing.T) {
	if got, want := NewWeightedRoundRobin().Name(), "weighted_round_robin"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// TestWeightedRoundRobinIsConcurrencySafe fails under -race if the credits are
// not guarded, and fails on the totals if a selection is lost.
func TestWeightedRoundRobinIsConcurrencySafe(t *testing.T) {
	const (
		goroutines         = 50
		requestsPerRoutine = 120
	)

	backends := weighted(1, 2, 3)
	strategy := NewWeightedRoundRobin()

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

	requests := goroutines * requestsPerRoutine
	for _, backend := range backends {
		if got, want := total[backend.Addr], requests*backend.Weight/6; got != want {
			t.Errorf("%s served %d requests, want exactly %d", backend.Addr, got, want)
		}
	}
}
