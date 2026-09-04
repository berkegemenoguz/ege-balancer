package balancer

import (
	"sync/atomic"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// RoundRobin walks the backends in order, handing each one an equal share of
// the requests. Selection is O(1): it only advances a counter.
type RoundRobin struct {
	// next is the position of the following selection. It is allowed to wrap
	// around; only its value modulo the pool size matters.
	next atomic.Uint64
}

// NewRoundRobin returns a round robin strategy.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Select returns the backend whose turn it is.
func (r *RoundRobin) Select(backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}
	// Add returns the incremented value, so the first call must map to index 0.
	index := (r.next.Add(1) - 1) % uint64(len(backends))
	return backends[index], nil
}

// Name identifies the strategy in configuration and logs.
func (r *RoundRobin) Name() string {
	return string(config.RoundRobin)
}
