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

// failingEveryAttempt sends n requests through strategy over backends while
// failing fails every attempt, retrying as the proxy does: once, among the
// backends not yet tried, by SelectRetry when the strategy takes turns. It
// records the backend each request was first offered to, and the one that
// served each retry.
func failingEveryAttempt(t *testing.T, strategy LBStrategy, backends []*Backend, failing *Backend, n int) (first, retried map[string]int) {
	t.Helper()

	retry := strategy.Select
	if turns, takesTurns := strategy.(RetryStrategy); takesTurns {
		retry = turns.SelectRetry
	}

	first = make(map[string]int, len(backends))
	retried = make(map[string]int, len(backends))
	for range n {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		first[backend.Addr]++
		if backend != failing {
			continue
		}
		backend, err = retry(without(backends, failing))
		if err != nil {
			t.Fatalf("the retry returned an unexpected error: %v", err)
		}
		retried[backend.Addr]++
	}
	return first, retried
}

func TestRoundRobinRetryLeavesTheNextTurnAlone(t *testing.T) {
	backends := pool(3)
	strategy := NewRoundRobin()

	// The first request goes to a, the second to b, which fails.
	_, _ = strategy.Select(backends)
	failed, _ := strategy.Select(backends)

	retry, err := strategy.SelectRetry(without(backends, failed))
	if err != nil {
		t.Fatalf("SelectRetry returned an unexpected error: %v", err)
	}
	if retry == failed {
		t.Fatalf("the retry went back to %s, which had just failed", failed.Addr)
	}

	if next, _ := strategy.Select(backends); next != backends[2] {
		t.Errorf("the request after the retry went to %s, want %s, whose turn it was",
			next.Addr, backends[2].Addr)
	}
}

// TestRoundRobinKeepsEveryTurnWhileABackendFails is the regression test for
// retries that took a turn: the backend after a failing one then lost its turn
// every time the other failed, and with every attempt failing it was never
// offered a request at all.
func TestRoundRobinKeepsEveryTurnWhileABackendFails(t *testing.T) {
	const requests = 1000

	backends := pool(10)
	first, retried := failingEveryAttempt(t, NewRoundRobin(), backends, backends[1], requests)

	want := requests / len(backends)
	for _, backend := range backends {
		if got := first[backend.Addr]; got != want {
			t.Errorf("%s was first offered %d requests, want %d", backend.Addr, got, want)
		}
	}

	// The failing backend's hundred retries go to the other nine, evenly.
	fewest, most := requests, 0
	for _, backend := range without(backends, backends[1]) {
		fewest = min(fewest, retried[backend.Addr])
		most = max(most, retried[backend.Addr])
	}
	if most-fewest > 1 {
		t.Errorf("the other backends served between %d and %d retries each, want them within one of each other",
			fewest, most)
	}
}

func TestRoundRobinRetryOnEmptyPool(t *testing.T) {
	if _, err := NewRoundRobin().SelectRetry(nil); err != ErrNoBackends {
		t.Errorf("error = %v, want ErrNoBackends", err)
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
