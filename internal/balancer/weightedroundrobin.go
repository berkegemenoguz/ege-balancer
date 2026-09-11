package balancer

import (
	"sync"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// WeightedRoundRobin hands out requests in proportion to the configured
// weights, using the smooth weighted round robin algorithm: every selection
// raises each backend's credit by its weight, the backend with the most credit
// serves the request, and its credit then drops by the total weight.
//
// Compared to picking a backend at random in proportion to its weight, this
// hits the configured ratio exactly and spreads the heavier backend's turns
// across the cycle instead of bunching them together.
type WeightedRoundRobin struct {
	// Selection is a read-modify-write over the whole pool, so it is guarded by
	// a mutex rather than by atomics.
	mu sync.Mutex
	// credit is keyed by backend address, which stays stable while the pool the
	// strategy is asked about changes, for instance when unhealthy backends are
	// filtered out.
	credit map[string]int
}

// NewWeightedRoundRobin returns a weighted round robin strategy.
func NewWeightedRoundRobin() *WeightedRoundRobin {
	return &WeightedRoundRobin{credit: make(map[string]int)}
}

// Select returns the backend whose turn it is under the configured weights.
func (w *WeightedRoundRobin) Select(backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var (
		chosen      *Backend
		totalWeight int
	)
	for _, backend := range backends {
		weight := backend.Weight()
		if weight < 1 {
			// Configuration defaults an omitted weight to 1; guard against a
			// pool built by hand so that a zero weight cannot starve selection.
			weight = 1
		}
		totalWeight += weight

		w.credit[backend.Addr] += weight
		if chosen == nil || w.credit[backend.Addr] > w.credit[chosen.Addr] {
			chosen = backend
		}
	}

	w.credit[chosen.Addr] -= totalWeight
	return chosen, nil
}

// Name identifies the strategy in configuration and logs.
func (w *WeightedRoundRobin) Name() string {
	return string(config.WeightedRoundRobin)
}
