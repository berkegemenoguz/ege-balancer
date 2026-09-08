package proxy

import (
	"log"
	"sync"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// breaker keeps a backend that fails repeatedly out of selection for a while,
// independently of health checking. After the open period one request is let
// through as a probe: if it succeeds the backend is taken back, otherwise the
// backend is shut out for another period.
//
// It is inert unless the circuit_breaker failure policy is configured, so the
// proxy can call it unconditionally.
type breaker struct {
	enabled          bool
	failureThreshold int
	openDuration     time.Duration

	mu       sync.Mutex
	circuits map[string]*circuit
}

// circuit is the breaker state of a single backend.
type circuit struct {
	failures int
	// openedUntil is when the backend may be probed again; the zero time means
	// the circuit is closed.
	openedUntil time.Time
	// probing is set while a single request is allowed through to test whether
	// the backend has recovered.
	probing bool
}

// newBreaker returns a breaker that is active only under the circuit_breaker
// failure policy.
func newBreaker(policy config.FailurePolicy, cfg config.CircuitBreaker) *breaker {
	return &breaker{
		enabled:          policy == config.CircuitBreakerPolicy,
		failureThreshold: cfg.FailureThreshold,
		openDuration:     time.Duration(cfg.OpenDuration),
		circuits:         make(map[string]*circuit),
	}
}

// allow reports whether the backend at addr may receive a request now.
func (b *breaker) allow(addr string) bool {
	if !b.enabled {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	tracked, known := b.circuits[addr]
	if !known || tracked.openedUntil.IsZero() {
		return true
	}
	if time.Now().Before(tracked.openedUntil) {
		return false
	}
	if tracked.probing {
		// A probe is already on its way; hold everything else back until it
		// reports its outcome.
		return false
	}

	tracked.probing = true
	log.Printf("breaker: probing %s after the open period", addr)
	return true
}

// recordSuccess closes the circuit of a backend that served a request.
func (b *breaker) recordSuccess(addr string) {
	if !b.enabled {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	tracked, known := b.circuits[addr]
	if !known {
		return
	}
	if !tracked.openedUntil.IsZero() {
		log.Printf("breaker: %s recovered and is back in the pool", addr)
	}
	delete(b.circuits, addr)
}

// recordFailure counts a failed request and opens the circuit once the backend
// has failed often enough in a row.
func (b *breaker) recordFailure(addr string) {
	if !b.enabled {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	tracked, known := b.circuits[addr]
	if !known {
		tracked = &circuit{}
		b.circuits[addr] = tracked
	}

	tracked.probing = false
	tracked.failures++
	if tracked.failures >= b.failureThreshold {
		tracked.openedUntil = time.Now().Add(b.openDuration)
		log.Printf("breaker: %s shut out for %s after %d consecutive failures",
			addr, b.openDuration, tracked.failures)
	}
}
