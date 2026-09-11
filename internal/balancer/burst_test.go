package balancer

import "testing"

// TestLeastConnectionsSpreadsABurst models requests that arrive together: each
// one reads the counters before any of them has acquired its backend. A scan
// for the least busy backend sends the whole burst to the same one.
func TestLeastConnectionsSpreadsABurst(t *testing.T) {
	const burst = 50

	backends := pool(10)
	strategy := seeded()

	chosen := make([]*Backend, 0, burst)
	for range burst {
		backend, err := strategy.Select(backends)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		chosen = append(chosen, backend)
	}
	for _, backend := range chosen {
		backend.Acquire()
	}

	var busiest int64
	for _, backend := range backends {
		busiest = max(busiest, backend.ActiveConnections())
	}
	t.Logf("a burst of %d over %d backends put at most %d on one backend", burst, len(backends), busiest)

	if busiest > burst/5 {
		t.Errorf("one backend received %d of a burst of %d, want the burst spread", busiest, burst)
	}
}
